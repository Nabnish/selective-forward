	package main

	import (
		"fmt"
		"net/http"
		"github.com/gorilla/websocket"
		"github.com/pion/webrtc/v3"
		"encoding/json"
		"log"
		"sync"

	)


	type msg struct{
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}


	var upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {return true},
	}

	func main(){
		http.Handle("/", http.FileServer(http.Dir("web/public")))
		http.HandleFunc("/ws",handleWS)
		fmt.Println("server started")
		http.ListenAndServe(":8080",nil)
	}

func handleWS(W http.ResponseWriter,r *http.Request){
		conn , err := upgrader.Upgrade(W,r,nil)
		
		if(err!=nil){
			log.Println("upgrade:",err)
			return
		}
		defer conn.Close()
		var wsMu sync.Mutex
	send := func(m msg) {
		wsMu.Lock()
		defer wsMu.Unlock()
		if err := conn.WriteJSON(m); err != nil {
			log.Println("write:", err)
		}
	}
		
		pc , err :=webrtc.NewPeerConnection(webrtc.Configuration{
			ICEServers : []webrtc.ICEServer{{URLs  : []string{"stun:stun.l.google.com:19302"}}},
		})

		if err!=nil{
			log.Println("connection failed",err)
			return 
		}
		defer pc.Close()

		pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
			log.Println("connection state:", s)
		})
		pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			log.Println("got track:", track.Kind(), track.Codec().MimeType)
		
			buf := make([]byte, 1500)
			for {
				n, _, err := track.Read(buf)
				if err != nil {
					log.Println("track read ended:", err)
					return
				}
				_ = n // we're not forwarding yet — just proving packets arrive
			}
		})
		pc.OnICECandidate(func(c *webrtc.ICECandidate) {
			if c == nil {
				return
			}
			b, _ := json.Marshal(c.ToJSON())
			send(msg{Event: "candidate", Data: b})
		})
		for{
			var message msg
			if err := conn.ReadJSON(&message); err!=nil{
				log.Println("error",err)
				return
			}
			log.Println("got event",message.Event)
			switch message.Event {
			case "offer":
				var offer webrtc.SessionDescription
				if err := json.Unmarshal(message.Data, &offer); err != nil {
					log.Println("bad offer:", err)

					
					continue
				}
				if err := pc.SetRemoteDescription(offer); err != nil {
					log.Println("SetRemoteDescription:", err)
					continue
				}
				answer, err := pc.CreateAnswer(nil)
				if err != nil {
					log.Println("CreateAnswer:", err)
					continue
				}
				if err := pc.SetLocalDescription(answer); err != nil {
					log.Println("SetLocalDescription:", err)
					continue
				}
				b, _ := json.Marshal(pc.LocalDescription())
				send(msg{Event: "answer", Data: b})
				log.Println("sent answer")
			
			case "candidate":
				var c webrtc.ICECandidateInit
				if err := json.Unmarshal(message.Data, &c); err != nil {
					log.Println("bad candidate:", err)
					continue
				}
				if err := pc.AddICECandidate(c); err != nil {
					log.Println("AddICECandidate:", err)
				}
			}
		}
	}
		


