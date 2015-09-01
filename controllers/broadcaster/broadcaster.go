package broadcaster

import (
	"time"

	"github.com/TF2Stadium/Helen/helpers"
	"github.com/googollee/go-socket.io"
)

type broadcastMessage struct {
	Room    string
	SteamId string
	Event   string
	Content string
}

var SteamIdSocketMap = make(map[string]*socketio.Socket)
var broadcasterTicker *time.Ticker
var broadcastStopChannel chan bool
var broadcastMessageChannel chan broadcastMessage
var socketServer *socketio.Server

func Init(server *socketio.Server) {
	broadcasterTicker = time.NewTicker(time.Millisecond * 1000)
	broadcastStopChannel = make(chan bool)
	broadcastMessageChannel = make(chan broadcastMessage)
	socketServer = server
	go broadcaster()
}

func Stop() {
	broadcasterTicker.Stop()
	broadcastStopChannel <- true
}

func SendMessage(steamid string, event string, content string) {
	broadcastMessageChannel <- broadcastMessage{
		Room:    "",
		SteamId: steamid,
		Event:   event,
		Content: content,
	}
}

func SendMessageToRoom(room string, event string, content string) {
	broadcastMessageChannel <- broadcastMessage{
		Room:    room,
		SteamId: "",
		Event:   event,
		Content: content,
	}
}

func broadcaster() {
	for {
		select {
		case message := <-broadcastMessageChannel:
			if message.Room == "" {
				socket, ok := SteamIdSocketMap[message.SteamId]
				if !ok {
					helpers.Logger.Warning("Failed to get user's socket: %d", message.SteamId)
					continue
				}
				(*socket).Emit(message.Event, message.Content)
			} else {
				socketServer.BroadcastTo(message.Room, message.Event, message.Content)
			}
		case <-broadcastStopChannel:
			return
		}
	}
}