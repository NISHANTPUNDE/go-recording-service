package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
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
	RecordingCh chan *media.Sample
	Recording   bool
	mu          sync.Mutex
}

// Client represents a connected peer
type Client struct {
	ID         string
	Role       string // "admin" or "user"
	PC         *webrtc.PeerConnection
	AudioTrack *webrtc.TrackRemote
}

var rooms = make(map[string]*Room)
var roomsMu sync.Mutex

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	})

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

		msgType := message["type"].(string)
		log.Printf("[GO-SFU] Received: %s", msgType)

		switch msgType {
		case "join-room":
			handleJoinRoom(conn, message)
		case "offer":
			handleOffer(conn, message)
		case "ice-candidate":
			handleICECandidate(conn, message)
		}
	}
}

func handleJoinRoom(conn *websocket.Conn, msg map[string]interface{}) {
	roomID := msg["roomId"].(string)
	clientID := msg["clientId"].(string)
	role := msg["role"].(string) // "admin" or "user"

	roomsMu.Lock()
	room, exists := rooms[roomID]
	if !exists {
		room = &Room{
			ID:          roomID,
			Clients:     make(map[string]*Client),
			RecordingCh: make(chan *media.Sample, 1000),
		}
		rooms[roomID] = room
		// Start recording goroutine
		go recordRoom(room)
	}
	roomsMu.Unlock()

	// Create peer connection
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		log.Println("PC creation error:", err)
		return
	}

	client := &Client{
		ID:   clientID,
		Role: role,
		PC:   pc,
	}

	room.mu.Lock()
	room.Clients[clientID] = client
	if role == "admin" {
		room.AdminID = clientID
	}
	room.mu.Unlock()

	// Handle incoming tracks
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("[GO-SFU] Track received from %s: %s", clientID, track.Kind().String())
		client.AudioTrack = track

		// Read audio and send to recording channel
		go func() {
			for {
				rtp, _, err := track.ReadRTP()
				if err != nil {
					if err == io.EOF {
						break
					}
					log.Println("RTP read error:", err)
					break
				}
				// Forward to recording
				sample := &media.Sample{
					Data:     rtp.Payload,
					Duration: time.Millisecond * 20,
				}
				select {
				case room.RecordingCh <- sample:
				default:
				}

				// Selective forwarding based on roles
				forwardAudio(room, clientID, role, rtp.Payload)
			}
		}()
	})

	// Send ICE candidates back
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

	// Send success response
	response := map[string]interface{}{
		"type":     "joined",
		"roomId":   roomID,
		"clientId": clientID,
	}
	data, _ := json.Marshal(response)
	conn.WriteMessage(websocket.TextMessage, data)
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

	room.mu.Lock()
	client := room.Clients[clientID]
	room.mu.Unlock()

	if client == nil {
		return
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpString,
	}

	err := client.PC.SetRemoteDescription(offer)
	if err != nil {
		log.Println("SetRemoteDescription error:", err)
		return
	}

	answer, err := client.PC.CreateAnswer(nil)
	if err != nil {
		log.Println("CreateAnswer error:", err)
		return
	}

	err = client.PC.SetLocalDescription(answer)
	if err != nil {
		log.Println("SetLocalDescription error:", err)
		return
	}

	response := map[string]interface{}{
		"type": "answer",
		"sdp":  answer.SDP,
	}
	data, _ := json.Marshal(response)
	conn.WriteMessage(websocket.TextMessage, data)
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

	room.mu.Lock()
	client := room.Clients[clientID]
	room.mu.Unlock()

	if client == nil {
		return
	}

	candidate := webrtc.ICECandidateInit{
		Candidate: candidateData["candidate"].(string),
	}

	client.PC.AddICECandidate(candidate)
}

// forwardAudio implements role-based audio routing
func forwardAudio(room *Room, senderID string, senderRole string, payload []byte) {
	room.mu.Lock()
	defer room.mu.Unlock()

	for clientID, client := range room.Clients {
		if clientID == senderID {
			continue
		}

		// Role-based routing:
		// - Admin audio goes to everyone
		// - User audio goes only to admin
		if senderRole == "admin" {
			// Admin speaks to all
			// TODO: Send to this client
		} else if senderRole == "user" {
			// User speaks only to admin
			if client.Role != "admin" {
				continue
			}
			// TODO: Send to admin
		}
	}
}

// recordRoom saves all audio to file using FFmpeg
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

	for sample := range room.RecordingCh {
		oggFile.WriteRTP(&rtp.Packet{
			Payload: sample.Data,
		})
	}

	log.Printf("[GO-SFU] Recording stopped: %s", filename)

	// Convert to WAV using FFmpeg
	wavFilename := filename[:len(filename)-4] + ".wav"
	cmd := exec.Command("ffmpeg", "-i", filename, "-y", wavFilename)
	cmd.Run()
}
