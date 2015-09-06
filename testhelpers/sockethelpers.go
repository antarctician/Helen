package testhelpers

import (
	"errors"
	"fmt"
	"github.com/TF2Stadium/Helen/config/stores"
	"github.com/TF2Stadium/Helen/controllers/broadcaster"
	"github.com/TF2Stadium/Helen/models"
	"github.com/bitly/go-simplejson"
	"github.com/googollee/go-socket.io"
	"github.com/gorilla/sessions"
	"math/rand"
	"net/http"
	"time"
)

// taken from http://stackoverflow.com/questions/22892120/how-to-generate-a-random-string-of-a-fixed-length-in-golang
var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func randSeq(n int) string {
	rand.Seed(time.Now().UTC().UnixNano())
	b := make([]rune, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

type fakeSocketServer struct {
	sockets []*fakeSocket
	rooms   map[string]map[string]socketio.Socket
}

func (f *fakeSocketServer) BroadcastTo(room string, message string, args ...interface{}) {
	for _, rooms := range f.rooms {
		socket, ok := rooms[room]
		if !ok {
			continue
		}
		socket.Emit(message, args[0].(string))
	}
	return
}

var FakeSocketServer = fakeSocketServer{nil, make(map[string]map[string]socketio.Socket)}

type message struct {
	event   string
	content string
}

type fakeSocket struct {
	id            string
	receivedQueue chan message
	eventHandlers map[string]interface{}
	server        *fakeSocketServer
}

func NewFakeSocket() *fakeSocket {
	so := &fakeSocket{
		id:            randSeq(5),
		receivedQueue: make(chan message, 100),
		eventHandlers: make(map[string]interface{}),
		server:        &FakeSocketServer,
	}

	FakeSocketServer.sockets = append(FakeSocketServer.sockets, so)
	return so
}

func (f *fakeSocket) GetNextMessage() (string, *simplejson.Json) {
	select {
	case s := <-f.receivedQueue:
		json, _ := simplejson.NewJson([]byte(s.content))
		return s.event, json
	default:
		return "", nil
	}
}

func (f *fakeSocket) Id() string {
	return f.id
}

func (f *fakeSocket) SimRequest(event string, args string) (*simplejson.Json, error) {
	fn, ok := f.eventHandlers[event]
	if !ok {
		return nil, errors.New("event not recognized")
	}

	fnAsserted, ok2 := fn.(func(string) string)
	if ok2 {
		return simplejson.NewJson([]byte(fnAsserted(args)))
	}

	// doesn't have to return string
	fnAsserted2 := fn.(func(string))
	fnAsserted2(args)
	return nil, nil
}

func (f *fakeSocket) Join(room string) error {
	arr, ok := f.server.rooms[f.Id()]
	if !ok {
		arr = make(map[string]socketio.Socket)
		f.server.rooms[f.Id()] = arr
	}
	arr[room] = f
	return nil
}

func (f *fakeSocket) Leave(room string) error {
	arr, ok := f.server.rooms[f.Id()]
	if ok {
		delete(arr, room)
	}
	return nil
}

func (f *fakeSocket) Rooms() []string {
	var rooms []string
	arr, ok := f.server.rooms[f.Id()]
	if !ok {
		return rooms
	}
	for room, _ := range arr {
		rooms = append(rooms, room)
	}
	return rooms
}

func (f *fakeSocket) On(message string, fn interface{}) error {
	f.eventHandlers[message] = fn
	return nil
}

func (f *fakeSocket) Emit(event string, args ...interface{}) error {
	// ASSUMES A STRING IS EMITTED
	f.receivedQueue <- message{event, args[0].(string)}
	return nil
}

func (f *fakeSocket) BroadcastTo(room, message string, args ...interface{}) error {
	for sid, rooms := range f.server.rooms {
		if sid == f.Id() {
			continue
		}

		socket, ok := rooms[room]
		if !ok {
			continue
		}
		socket.Emit(message, args[0].(string))
	}
	return nil
}

func (f *fakeSocket) Request() *http.Request {
	return &http.Request{}
}

func (f *fakeSocket) FakeAuthenticate(player *models.Player) *http.Request {
	session := &sessions.Session{
		ID:      randSeq(5),
		Values:  make(map[interface{}]interface{}),
		Options: nil,
		IsNew:   false,
	}

	session.Values["id"] = fmt.Sprint(player.ID)
	session.Values["steamid"] = fmt.Sprint(player.SteamId)
	session.Values["role"] = player.Role

	broadcaster.SteamIdSocketMap[player.SteamId] = f
	stores.SocketAuthStore[f.Id()] = session
	return nil
}
