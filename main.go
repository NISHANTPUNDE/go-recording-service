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
	RecordingCh chan *rtp.Packet
	Recording   bool
	mu          sync.RWMutex
}

// Client represents a connected peer
type Client struct {
	ID          string
	Role        string // "admin" or "user"
	Conn        *websocket.Conn
	PC          *webrtc.PeerConnection
	AudioTrack  *webrtc.TrackRemote
	OutputTrack *webrtc.TrackLocalStaticRTP // Track to send audio TO this client
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
		room = &Room{
			ID:          roomID,
			Clients:     make(map[string]*Client),
			RecordingCh: make(chan *rtp.Packet, 1000),
		}
		rooms[roomID] = room
		go recordRoom(room)
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

	// Create output track for this client (to send audio TO them)
	outputTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		fmt.Sprintf("audio-%s", clientID),
		fmt.Sprintf("stream-%s", clientID),
	)
	if err != nil {
		log.Println("Track creation error:", err)
		return
	}

	// Add the output track to peer connection
	sender, err := pc.AddTrack(outputTrack)
	if err != nil {
		log.Println("AddTrack error:", err)
		return
	}

	// Read RTCP (required for WebRTC)
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

	// Handle incoming audio track from this client
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("[GO-SFU] Audio track received from %s (role: %s)", clientID, role)
		client.AudioTrack = track

		// Forward audio to other clients
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

				// Send to recording
				select {
				case room.RecordingCh <- rtpPacket:
				default:
				}

				// Forward to other clients based on roles
				forwardAudioPacket(room, clientID, role, rtpPacket)
			}
		}()
	})

	// Send ICE candidates
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

	// Send joined response
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

	// If room is empty, clean up
	if len(room.Clients) == 0 {
		close(room.RecordingCh)
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
			continue // Don't send to self
		}

		// Role-based routing:
		// - Admin audio goes to everyone
		// - User audio goes only to admin
		shouldForward := false
		if senderRole == "admin" {
			// Admin speaks to all
			shouldForward = true
		} else if senderRole == "user" {
			// User speaks only to admin
			if client.Role == "admin" {
				shouldForward = true
			}
		}

		if shouldForward && client.OutputTrack != nil {
			if err := client.OutputTrack.WriteRTP(packet); err != nil {
				log.Printf("WriteRTP error to %s: %v", clientID, err)
			}
		}
	}
}

// recordRoom saves all audio to OGG file
func recordRoom(room *Room) {
	filename := fmt.Sprintf("recordings/%s_%d.ogg", room.ID, time.Now().Unix())
	os.MkdirAll("recordings", 0755)

	oggFile, err := oggwriter.New(filename, 48000, 2)
	if err != nil {
		log.Println("OGG writer error:", err)
		return
	}
	defer oggFile.Close()

	log.Printf("[GO-SFU] Recording started: %s", filename)
	room.Recording = true

	for packet := range room.RecordingCh {
		if err := oggFile.WriteRTP(packet); err != nil {
			log.Println("OGG write error:", err)
		}
	}

	log.Printf("[GO-SFU] Recording stopped: %s", filename)

	// Convert to WAV using FFmpeg
	wavFilename := filename[:len(filename)-4] + ".wav"
	cmd := exec.Command("ffmpeg", "-y", "-i", filename, wavFilename)
	if err := cmd.Run(); err != nil {
		log.Println("FFmpeg conversion error:", err)
	} else {
		log.Printf("[GO-SFU] Converted to: %s", wavFilename)
	}
}

// Recording represents a recording file for JSON response
type RecordingInfo struct {
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"createdAt"`
	URL       string `json:"url"`
}

// handleListRecordings returns all recordings as JSON
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
		// Only include audio files
		name := file.Name()
		if !strings.HasSuffix(name, ".ogg") && !strings.HasSuffix(name, ".wav") {
			continue
		}
		recordings = append(recordings, RecordingInfo{
			Filename:  name,
			Size:      info.Size(),
			CreatedAt: info.ModTime().Format(time.RFC3339),
			URL:       "/recordings/" + name,
		})
	}

	// Sort by time descending (newest first)
	sort.Slice(recordings, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, recordings[i].CreatedAt)
		tj, _ := time.Parse(time.RFC3339, recordings[j].CreatedAt)
		return ti.After(tj)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"recordings": recordings})
}

// handleRecordingFile serves or deletes a specific recording file
func handleRecordingFile(w http.ResponseWriter, r *http.Request) {
	// Extract filename from path: /recordings/{filename}
	filename := strings.TrimPrefix(r.URL.Path, "/recordings/")
	if filename == "" {
		http.Error(w, "Filename required", http.StatusBadRequest)
		return
	}

	// Security: prevent directory traversal
	if strings.Contains(filename, "..") || strings.Contains(filename, "/") {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}

	filepath := "recordings/" + filename

	switch r.Method {
	case "GET":
		// Serve the file
		http.ServeFile(w, r, filepath)
	case "DELETE":
		// Delete the file
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
