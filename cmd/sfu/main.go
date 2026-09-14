package main

import (
	"fmt"
	"net/http"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"encoding/json"
	"log"

)

type msg struct{
	Event string  `json."event"`
	Data json.RawMessage  `json."data"`
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

	for{
		var message msg
		if err := conn.ReadJSON(&message); err!=nil{
			log.Println("error",err)
			return
		}
		log.Println("got event",message.Event)
		if message.Event == "offer"{
			var offer webrtc.SessionDescription
			if err := json.Unmarshal(message.Data,&offer); err!=nil{
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
			conn.WriteJSON(msg{Event: "answer", Data: b})
			log.Println("sent answer")
		}
	}
}
	


