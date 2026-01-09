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
	ID        string
	Clients   map[string]*Client
	AdminID   string
	BaseDir   string // Directory for this room's recordings
	Recording bool
	mu        sync.RWMutex
}

// Client represents a connected peer
type Client struct {
	ID          string
	Role        string // "admin" or "user"
	Conn        *websocket.Conn
	PC          *webrtc.PeerConnection
	AudioTrack  *webrtc.TrackRemote
	OutputTrack *webrtc.TrackLocalStaticRTP
	OggWriter   *oggwriter.OggWriter
	OggFilename string
	PacketCount int64
	mu          sync.Mutex
}

var rooms = make(map[string]*Room)
var roomsMu sync.Mutex

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

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
		os.MkdirAll("recordings", 0755)
		room = &Room{
			ID:        roomID,
			Clients:   make(map[string]*Client),
			BaseDir:   fmt.Sprintf("recordings/%s_%d", roomID, time.Now().Unix()),
			Recording: true,
		}
		rooms[roomID] = room
		log.Printf("[GO-SFU] Room created: %s", roomID)
	}
	roomsMu.Unlock()

	// Create per-client OGG file
	os.MkdirAll(room.BaseDir, 0755)
	oggFilename := fmt.Sprintf("%s/%s_%s.ogg", room.BaseDir, role, clientID[:8])
	oggFile, err := oggwriter.New(oggFilename, 48000, 2)
	if err != nil {
		log.Println("OGG writer error:", err)
		return
	}
	log.Printf("[GO-SFU] Recording started for %s: %s", role, oggFilename)

	// Create peer connection
	m := &webrtc.MediaEngine{}
	m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio)

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := api.NewPeerConnection(config)
	if err != nil {
		log.Println("PC creation error:", err)
		oggFile.Close()
		return
	}

	outputTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		fmt.Sprintf("audio-%s", clientID),
		fmt.Sprintf("stream-%s", clientID),
	)
	if err != nil {
		log.Println("Track creation error:", err)
		oggFile.Close()
		return
	}

	sender, err := pc.AddTrack(outputTrack)
	if err != nil {
		log.Println("AddTrack error:", err)
		oggFile.Close()
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
		OggWriter:   oggFile,
		OggFilename: oggFilename,
	}

	room.mu.Lock()
	room.Clients[clientID] = client
	if role == "admin" {
		room.AdminID = clientID
	}
	room.mu.Unlock()

	// Handle incoming audio
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("[GO-SFU] Audio track from %s (role: %s)", clientID, role)
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

				// Write to this client's own OGG file
				client.mu.Lock()
				if client.OggWriter != nil {
					client.OggWriter.WriteRTP(rtpPacket)
					client.PacketCount++
				}
				client.mu.Unlock()

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
		log.Printf("[GO-SFU] Client %s state: %s", clientID, state.String())
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

	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdpString}
	if err := client.PC.SetRemoteDescription(offer); err != nil {
		log.Println("SetRemoteDescription error:", err)
		return
	}

	answer, _ := client.PC.CreateAnswer(nil)
	client.PC.SetLocalDescription(answer)

	response := map[string]interface{}{"type": "answer", "sdp": answer.SDP}
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

	candidate := webrtc.ICECandidateInit{Candidate: candidateData["candidate"].(string)}
	client.PC.AddICECandidate(candidate)
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
		// Close OGG file for this client
		client.mu.Lock()
		if client.OggWriter != nil {
			client.OggWriter.Close()
			log.Printf("[GO-SFU] Recording stopped for %s (packets: %d)", client.Role, client.PacketCount)
		}
		client.mu.Unlock()
		client.PC.Close()
		delete(room.Clients, clientID)
	}

	// If room empty, merge all recordings
	if len(room.Clients) == 0 {
		room.Recording = false
		go mergeRecordings(room.BaseDir, room.ID)
		roomsMu.Lock()
		delete(rooms, roomID)
		roomsMu.Unlock()
	}
	room.mu.Unlock()

	log.Printf("[GO-SFU] Client %s left room %s", clientID, roomID)
}

// mergeRecordings combines all OGG files in a directory into one MP3
func mergeRecordings(baseDir string, roomID string) {
	files, err := os.ReadDir(baseDir)
	if err != nil {
		log.Println("ReadDir error:", err)
		return
	}

	var oggFiles []string
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".ogg") {
			oggFiles = append(oggFiles, baseDir+"/"+f.Name())
		}
	}

	if len(oggFiles) == 0 {
		log.Println("[GO-SFU] No OGG files to merge")
		return
	}

	outputFile := fmt.Sprintf("recordings/%s_merged.mp3", roomID)

	if len(oggFiles) == 1 {
		// Single file, just convert
		cmd := exec.Command("ffmpeg", "-y", "-i", oggFiles[0],
			"-acodec", "libmp3lame", "-ab", "128k", "-ar", "48000", "-ac", "1",
			outputFile)
		if err := cmd.Run(); err != nil {
			log.Println("FFmpeg convert error:", err)
		} else {
			log.Printf("[GO-SFU] Converted: %s", outputFile)
		}
	} else {
		// Multiple files, merge with amix filter
		args := []string{"-y"}
		for _, f := range oggFiles {
			args = append(args, "-i", f)
		}
		// Mix all inputs together
		filterComplex := fmt.Sprintf("amix=inputs=%d:duration=longest:dropout_transition=0", len(oggFiles))
		args = append(args, "-filter_complex", filterComplex,
			"-acodec", "libmp3lame", "-ab", "128k", "-ar", "48000", "-ac", "1",
			outputFile)

		cmd := exec.Command("ffmpeg", args...)
		if err := cmd.Run(); err != nil {
			log.Println("FFmpeg merge error:", err)
		} else {
			log.Printf("[GO-SFU] Merged %d files to: %s", len(oggFiles), outputFile)
		}
	}

	// Cleanup individual OGG files
	os.RemoveAll(baseDir)
}

func forwardAudioPacket(room *Room, senderID string, senderRole string, packet *rtp.Packet) {
	room.mu.RLock()
	defer room.mu.RUnlock()

	for clientID, client := range room.Clients {
		if clientID == senderID {
			continue
		}
		shouldForward := senderRole == "admin" || client.Role == "admin"
		if shouldForward && client.OutputTrack != nil {
			client.OutputTrack.WriteRTP(packet)
		}
	}
}

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
		info, _ := file.Info()
		name := file.Name()
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
	if filename == "" || strings.Contains(filename, "..") || strings.Contains(filename, "/") {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}

	filepath := "recordings/" + filename
	switch r.Method {
	case "GET":
		http.ServeFile(w, r, filepath)
	case "DELETE":
		if err := os.Remove(filepath); err != nil {
			http.Error(w, "Failed to delete", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"message": "Deleted"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
