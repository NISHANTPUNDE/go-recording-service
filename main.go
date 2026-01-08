package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Room holds all peer connections for a room
type Room struct {
	ID          string
	Clients     map[string]*Client
	AdminID     string
	OggWriter   *oggwriter.OggWriter
	OggFilename string
	Recording   bool
	PacketCount int64 // Count packets for debugging
	mu          sync.RWMutex
	writerMu    sync.Mutex // Separate mutex for OGG writer
}

// Client represents a connected peer
type Client struct {
	ID          string
	Role        string // "admin" or "user"
	Conn        *websocket.Conn
	PC          *webrtc.PeerConnection
	AudioTrack  *webrtc.TrackRemote
	OutputTrack *webrtc.TrackLocalStaticRTP
	mu          sync.Mutex
}

var rooms = make(map[string]*Room)
var roomsMu sync.Mutex

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	// CORS middleware
	corsHandler := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			h(w, r)
		}
	}

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

	// Recordings API
	http.HandleFunc("/recordings", corsHandler(handleListRecordings))
	http.HandleFunc("/recordings/", corsHandler(handleRecordingFile))

	log.Printf("[GO-SFU] Starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}
	defer conn.Close()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println("Read error:", err)
			break
		}

		var message map[string]interface{}
		if err := json.Unmarshal(msg, &message); err != nil {
			log.Println("JSON parse error:", err)
			continue
		}

		msgType, ok := message["type"].(string)
		if !ok {
			continue
		}
		log.Printf("[GO-SFU] Received: %s", msgType)

		switch msgType {
		case "join-room":
			handleJoinRoom(conn, message)
		case "offer":
			handleOffer(conn, message)
		case "ice-candidate":
			handleICECandidate(conn, message)
		case "leave-room":
			handleLeaveRoom(message)
		}
	}
}

func handleJoinRoom(conn *websocket.Conn, msg map[string]interface{}) {
	roomID := msg["roomId"].(string)
	clientID := msg["clientId"].(string)
	role := msg["role"].(string)

	roomsMu.Lock()
	room, exists := rooms[roomID]
	if !exists {
		// Create OGG file for recording
		os.MkdirAll("recordings", 0755)
		filename := fmt.Sprintf("recordings/%s_%d.ogg", roomID, time.Now().Unix())
		oggFile, err := oggwriter.New(filename, 48000, 2)
		if err != nil {
			log.Println("OGG writer error:", err)
			roomsMu.Unlock()
			return
		}

		room = &Room{
			ID:          roomID,
			Clients:     make(map[string]*Client),
			OggWriter:   oggFile,
			OggFilename: filename,
			Recording:   true,
		}
		rooms[roomID] = room
		log.Printf("[GO-SFU] Recording started: %s", filename)
	}
	roomsMu.Unlock()

	// Create peer connection with media engine
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		log.Println("RegisterCodec error:", err)
		return
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := api.NewPeerConnection(config)
	if err != nil {
		log.Println("PC creation error:", err)
		return
	}

	// Create output track for this client
	outputTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		fmt.Sprintf("audio-%s", clientID),
		fmt.Sprintf("stream-%s", clientID),
	)
	if err != nil {
		log.Println("Track creation error:", err)
		return
	}

	sender, err := pc.AddTrack(outputTrack)
	if err != nil {
		log.Println("AddTrack error:", err)
		return
	}

	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(rtcpBuf); err != nil {
				return
			}
		}
	}()

	client := &Client{
		ID:          clientID,
		Role:        role,
		Conn:        conn,
		PC:          pc,
		OutputTrack: outputTrack,
	}

	room.mu.Lock()
	room.Clients[clientID] = client
	if role == "admin" {
		room.AdminID = clientID
	}
	room.mu.Unlock()

	// Handle incoming audio track
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("[GO-SFU] Audio track received from %s (role: %s)", clientID, role)
		client.AudioTrack = track

		go func() {
			for {
				rtpPacket, _, err := track.ReadRTP()
				if err != nil {
					if err == io.EOF {
						log.Printf("[GO-SFU] Track ended for %s", clientID)
					} else {
						log.Println("RTP read error:", err)
					}
					break
				}

				// Write ALL audio to OGG file (both admin and user)
				if room.Recording && room.OggWriter != nil {
					room.writerMu.Lock()
					room.OggWriter.WriteRTP(rtpPacket)
					room.PacketCount++
					room.writerMu.Unlock()
				}

				// Forward to other clients
				forwardAudioPacket(room, clientID, role, rtpPacket)
			}
		}()
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			candidate := c.ToJSON()
			response := map[string]interface{}{
				"type":      "ice-candidate",
				"candidate": candidate,
			}
			data, _ := json.Marshal(response)
			conn.WriteMessage(websocket.TextMessage, data)
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[GO-SFU] Client %s connection state: %s", clientID, state.String())
	})

	response := map[string]interface{}{
		"type":     "joined",
		"roomId":   roomID,
		"clientId": clientID,
	}
	data, _ := json.Marshal(response)
	conn.WriteMessage(websocket.TextMessage, data)

	log.Printf("[GO-SFU] Client %s joined room %s as %s", clientID, roomID, role)
}

func handleOffer(conn *websocket.Conn, msg map[string]interface{}) {
	roomID := msg["roomId"].(string)
	clientID := msg["clientId"].(string)
	sdpString := msg["sdp"].(string)

	roomsMu.Lock()
	room := rooms[roomID]
	roomsMu.Unlock()
	if room == nil {
		return
	}

	room.mu.RLock()
	client := room.Clients[clientID]
	room.mu.RUnlock()
	if client == nil {
		return
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpString,
	}

	if err := client.PC.SetRemoteDescription(offer); err != nil {
		log.Println("SetRemoteDescription error:", err)
		return
	}

	answer, err := client.PC.CreateAnswer(nil)
	if err != nil {
		log.Println("CreateAnswer error:", err)
		return
	}

	if err := client.PC.SetLocalDescription(answer); err != nil {
		log.Println("SetLocalDescription error:", err)
		return
	}

	response := map[string]interface{}{
		"type": "answer",
		"sdp":  answer.SDP,
	}
	data, _ := json.Marshal(response)
	conn.WriteMessage(websocket.TextMessage, data)

	log.Printf("[GO-SFU] Answer sent to %s", clientID)
}

func handleICECandidate(conn *websocket.Conn, msg map[string]interface{}) {
	roomID := msg["roomId"].(string)
	clientID := msg["clientId"].(string)
	candidateData := msg["candidate"].(map[string]interface{})

	roomsMu.Lock()
	room := rooms[roomID]
	roomsMu.Unlock()
	if room == nil {
		return
	}

	room.mu.RLock()
	client := room.Clients[clientID]
	room.mu.RUnlock()
	if client == nil {
		return
	}

	candidate := webrtc.ICECandidateInit{
		Candidate: candidateData["candidate"].(string),
	}

	if err := client.PC.AddICECandidate(candidate); err != nil {
		log.Println("AddICECandidate error:", err)
	}
}

func handleLeaveRoom(msg map[string]interface{}) {
	roomID, ok := msg["roomId"].(string)
	if !ok {
		return
	}
	clientID, ok := msg["clientId"].(string)
	if !ok {
		return
	}

	roomsMu.Lock()
	room := rooms[roomID]
	roomsMu.Unlock()
	if room == nil {
		return
	}

	room.mu.Lock()
	client := room.Clients[clientID]
	if client != nil {
		client.PC.Close()
		delete(room.Clients, clientID)
	}

	// If room is empty, finalize recording
	if len(room.Clients) == 0 {
		room.Recording = false
		if room.OggWriter != nil {
			room.OggWriter.Close()
			log.Printf("[GO-SFU] Recording stopped: %s (packets: %d)", room.OggFilename, room.PacketCount)

			// Convert to MP3 with high quality settings
			go func(oggFile string, packets int64) {
				if packets == 0 {
					log.Printf("[GO-SFU] WARNING: No audio packets recorded for %s", oggFile)
					return
				}
				mp3File := oggFile[:len(oggFile)-4] + ".mp3"
				// High quality conversion: use libmp3lame, 128kbps, 48kHz
				cmd := exec.Command("ffmpeg", "-y", "-i", oggFile,
					"-acodec", "libmp3lame",
					"-ab", "128k",
					"-ar", "48000",
					"-ac", "1",
					mp3File)
				if err := cmd.Run(); err != nil {
					log.Println("FFmpeg error:", err)
				} else {
					log.Printf("[GO-SFU] Converted to: %s", mp3File)
				}
			}(room.OggFilename, room.PacketCount)
		}
		roomsMu.Lock()
		delete(rooms, roomID)
		roomsMu.Unlock()
	}
	room.mu.Unlock()

	log.Printf("[GO-SFU] Client %s left room %s", clientID, roomID)
}

// forwardAudioPacket forwards audio based on role rules
func forwardAudioPacket(room *Room, senderID string, senderRole string, packet *rtp.Packet) {
	room.mu.RLock()
	defer room.mu.RUnlock()

	for clientID, client := range room.Clients {
		if clientID == senderID {
			continue
		}

		shouldForward := false
		if senderRole == "admin" {
			shouldForward = true
		} else if senderRole == "user" {
			if client.Role == "admin" {
				shouldForward = true
			}
		}

		if shouldForward && client.OutputTrack != nil {
			if err := client.OutputTrack.WriteRTP(packet); err != nil {
				// Ignore write errors
			}
		}
	}
}

// RecordingInfo for JSON response
type RecordingInfo struct {
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"createdAt"`
	URL       string `json:"url"`
}

func handleListRecordings(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	files, err := os.ReadDir("recordings")
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"recordings": []RecordingInfo{}})
			return
		}
		http.Error(w, "Failed to read recordings", http.StatusInternalServerError)
		return
	}

	var recordings []RecordingInfo
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		info, err := file.Info()
		if err != nil {
			continue
		}
		name := file.Name()
		// Include MP3, OGG, and WAV files
		if !strings.HasSuffix(name, ".mp3") && !strings.HasSuffix(name, ".ogg") && !strings.HasSuffix(name, ".wav") {
			continue
		}
		recordings = append(recordings, RecordingInfo{
			Filename:  name,
			Size:      info.Size(),
			CreatedAt: info.ModTime().Format(time.RFC3339),
			URL:       "/recordings/" + name,
		})
	}

	sort.Slice(recordings, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, recordings[i].CreatedAt)
		tj, _ := time.Parse(time.RFC3339, recordings[j].CreatedAt)
		return ti.After(tj)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"recordings": recordings})
}

func handleRecordingFile(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/recordings/")
	if filename == "" {
		http.Error(w, "Filename required", http.StatusBadRequest)
		return
	}

	if strings.Contains(filename, "..") || strings.Contains(filename, "/") {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}

	filepath := "recordings/" + filename

	switch r.Method {
	case "GET":
		http.ServeFile(w, r, filepath)
	case "DELETE":
		if err := os.Remove(filepath); err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "Recording not found", http.StatusNotFound)
			} else {
				http.Error(w, "Failed to delete", http.StatusInternalServerError)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"message": "Recording deleted"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
