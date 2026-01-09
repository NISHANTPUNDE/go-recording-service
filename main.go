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
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Room holds all clients in a room
type Room struct {
	ID          string
	Clients     map[string]*Client
	AdminID     string
	AdminFolder string // recordings/{adminId}/
	Recording   bool
	mu          sync.RWMutex
}

// Client represents a connected user
type Client struct {
	ID           string
	Role         string // "admin" or "user"
	Conn         *websocket.Conn
	connMu       sync.Mutex // Mutex for WebSocket writes
	PC           *webrtc.PeerConnection
	AudioTrack   *webrtc.TrackRemote
	OutputTrack  *webrtc.TrackLocalStaticRTP
	OggWriter    *oggwriter.OggWriter
	OggFilename  string
	PacketCount  int64
	AdminOgg     *oggwriter.OggWriter // For users: write admin's audio here too
	AdminOggFile string
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

	log.Printf("[GO-SFU] Starting on port %s (scalable for 200 users)", port)
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
			continue
		}

		msgType, _ := message["type"].(string)
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
			ID:        roomID,
			Clients:   make(map[string]*Client),
			Recording: true,
		}
		rooms[roomID] = room
		log.Printf("[GO-SFU] Room created: %s", roomID)
	}

	// Set admin folder when admin joins
	if role == "admin" && room.AdminID == "" {
		room.AdminID = clientID[:8]
		room.AdminFolder = fmt.Sprintf("recordings/%s", room.AdminID)
		os.MkdirAll(room.AdminFolder, 0755)
	}
	roomsMu.Unlock()

	// Create per-client recording file
	var oggWriter *oggwriter.OggWriter
	var oggFilename string
	if room.AdminFolder != "" {
		oggFilename = fmt.Sprintf("%s/%s_%s_%s.ogg", room.AdminFolder, roomID, role, clientID[:8])
		var err error
		oggWriter, err = oggwriter.New(oggFilename, 48000, 2)
		if err != nil {
			log.Printf("[GO-SFU] OGG writer error: %v", err)
		} else {
			log.Printf("[GO-SFU] Recording started: %s", oggFilename)
		}
	}

	// Create WebRTC peer connection
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
		return
	}

	// Create output track for sending audio to this client
	outputTrack, _ := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		fmt.Sprintf("audio-%s", clientID),
		fmt.Sprintf("stream-%s", clientID),
	)

	sender, _ := pc.AddTrack(outputTrack)
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
		OggWriter:   oggWriter,
		OggFilename: oggFilename,
	}

	room.mu.Lock()
	room.Clients[clientID] = client
	room.mu.Unlock()

	// Handle incoming audio from this client
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("[GO-SFU] Audio track from %s (role: %s)", clientID, role)
		client.AudioTrack = track

		go func() {
			for {
				rtpPacket, _, err := track.ReadRTP()
				if err != nil {
					if err == io.EOF {
						log.Printf("[GO-SFU] Track ended for %s", clientID)
					}
					break
				}

				// Record this client's audio
				if client.OggWriter != nil {
					client.OggWriter.WriteRTP(rtpPacket)
					atomic.AddInt64(&client.PacketCount, 1)
				}

				// Forward audio based on role
				forwardAudio(room, clientID, role, rtpPacket)
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
			client.connMu.Lock()
			conn.WriteMessage(websocket.TextMessage, data)
			client.connMu.Unlock()
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[GO-SFU] Client %s state: %s", clientID, state.String())
	})

	// Send joined confirmation
	response := map[string]interface{}{
		"type":     "joined",
		"roomId":   roomID,
		"clientId": clientID,
	}
	data, _ := json.Marshal(response)
	client.connMu.Lock()
	conn.WriteMessage(websocket.TextMessage, data)
	client.connMu.Unlock()

	log.Printf("[GO-SFU] Client %s joined room %s as %s", clientID, roomID, role)
}

// forwardAudio routes audio based on role:
// - Admin audio → ALL users
// - User audio → Admin ONLY
func forwardAudio(room *Room, senderID string, senderRole string, packet *rtp.Packet) {
	room.mu.RLock()
	defer room.mu.RUnlock()

	for clientID, client := range room.Clients {
		if clientID == senderID {
			continue
		}

		shouldForward := false
		if senderRole == "admin" {
			// Admin broadcasts to ALL users
			shouldForward = true
		} else if senderRole == "user" && client.Role == "admin" {
			// Users only send to admin
			shouldForward = true
		}

		if shouldForward && client.OutputTrack != nil {
			client.OutputTrack.WriteRTP(packet)
		}
	}
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
	client.connMu.Lock()
	conn.WriteMessage(websocket.TextMessage, data)
	client.connMu.Unlock()
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
	roomID, _ := msg["roomId"].(string)
	clientID, _ := msg["clientId"].(string)
	if roomID == "" || clientID == "" {
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
		// Close recording for this client
		if client.OggWriter != nil {
			client.OggWriter.Close()
			log.Printf("[GO-SFU] Recording stopped for %s (packets: %d)", client.Role, client.PacketCount)
		}
		client.PC.Close()
		delete(room.Clients, clientID)
	}

	// If room is empty, finalize recordings
	if len(room.Clients) == 0 {
		room.Recording = false
		go finalizeRecordings(room.AdminFolder, roomID)
		roomsMu.Lock()
		delete(rooms, roomID)
		roomsMu.Unlock()
	}
	room.mu.Unlock()

	log.Printf("[GO-SFU] Client %s left room %s", clientID, roomID)
}

// finalizeRecordings creates merged recordings for admin
func finalizeRecordings(adminFolder string, roomID string) {
	if adminFolder == "" {
		return
	}
	time.Sleep(1 * time.Second) // Wait for files to be written

	files, err := os.ReadDir(adminFolder)
	if err != nil {
		log.Printf("[GO-SFU] ReadDir error: %v", err)
		return
	}

	// Find all OGG files for this room
	var oggFiles []string
	for _, f := range files {
		if strings.HasPrefix(f.Name(), roomID) && strings.HasSuffix(f.Name(), ".ogg") {
			oggFiles = append(oggFiles, adminFolder+"/"+f.Name())
		}
	}

	if len(oggFiles) == 0 {
		log.Println("[GO-SFU] No OGG files to merge")
		return
	}

	log.Printf("[GO-SFU] Merging %d files for room %s", len(oggFiles), roomID)
	outputFile := fmt.Sprintf("%s/%s_merged.mp3", adminFolder, roomID)

	var cmd *exec.Cmd
	if len(oggFiles) == 1 {
		cmd = exec.Command("ffmpeg", "-y", "-i", oggFiles[0],
			"-acodec", "libmp3lame", "-ab", "128k", "-ar", "48000", "-ac", "1",
			outputFile)
	} else {
		args := []string{"-y"}
		for _, f := range oggFiles {
			args = append(args, "-i", f)
		}
		filterComplex := fmt.Sprintf("amix=inputs=%d:duration=longest:dropout_transition=0", len(oggFiles))
		args = append(args, "-filter_complex", filterComplex,
			"-acodec", "libmp3lame", "-ab", "128k", "-ar", "48000", "-ac", "1",
			outputFile)
		cmd = exec.Command("ffmpeg", args...)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[GO-SFU] FFmpeg error: %v\nOutput: %s", err, string(output))
	} else {
		log.Printf("[GO-SFU] Merged to: %s", outputFile)
		// Cleanup OGG files
		for _, f := range oggFiles {
			os.Remove(f)
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

	var recordings []RecordingInfo

	adminDirs, err := os.ReadDir("recordings")
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"recordings": recordings})
			return
		}
		http.Error(w, "Failed to read recordings", http.StatusInternalServerError)
		return
	}

	for _, adminDir := range adminDirs {
		if !adminDir.IsDir() {
			if strings.HasSuffix(adminDir.Name(), ".mp3") {
				info, _ := adminDir.Info()
				recordings = append(recordings, RecordingInfo{
					Filename:  adminDir.Name(),
					Size:      info.Size(),
					CreatedAt: info.ModTime().Format(time.RFC3339),
					URL:       "/recordings/" + adminDir.Name(),
				})
			}
			continue
		}

		adminPath := "recordings/" + adminDir.Name()
		files, _ := os.ReadDir(adminPath)

		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".mp3") {
				continue
			}
			info, _ := file.Info()
			recordings = append(recordings, RecordingInfo{
				Filename:  file.Name(),
				Size:      info.Size(),
				CreatedAt: info.ModTime().Format(time.RFC3339),
				URL:       "/recordings/" + adminDir.Name() + "/" + file.Name(),
			})
		}
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
	path := strings.TrimPrefix(r.URL.Path, "/recordings/")
	if path == "" || strings.Contains(path, "..") {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	filepath := "recordings/" + path
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
