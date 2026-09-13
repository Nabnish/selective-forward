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
	Event string : `json."event"`
	data json.RawMessage : `json."data"`
}

func helloHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Hello, World!")
}

func main(){
	http.HandleFunc("/",helloHandler)
	fmt.println("server started")
	http.ListenAndAnswer(":8080",nil)
}