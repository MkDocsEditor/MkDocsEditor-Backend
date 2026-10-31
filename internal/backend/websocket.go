package backend

import (
	"encoding/json"
	"strings"

	"fmt"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/configuration"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"log"
	"net/http"
	mutexSync "sync"
)

const (
	TypeInitialContent = "initial-content"
	TypeSyncRequest    = "sync-request"
)

// SyncManager is implemented by the component that processes document sync requests
type SyncManager interface {
	IsItemBeingEditedRecursive(s *Section) (err error)
}

type WebsocketConnectionManager struct {
	treeManager *TreeManager

	upgrader websocket.Upgrader
	lock     mutexSync.RWMutex
	// connected clients (websocket -> document id)
	clients                map[*websocket.Conn]string
	connectionsPerDocument map[string]uint

	onNewClient          func(client *websocket.Conn, document *Document) error
	onSyncRequest        func(client *websocket.Conn, request SyncRequest) error
	onClientDisconnected func(client *websocket.Conn, documentId string, remainingConnections uint)
}

func NewWebsocketConnectionManager(
	treeManager *TreeManager,
) *WebsocketConnectionManager {
	return &WebsocketConnectionManager{
		treeManager: treeManager,

		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     checkOrigin(configuration.CurrentConfig.Server.CORS.AllowedOrigins),
		},
		lock:                   mutexSync.RWMutex{},
		clients:                make(map[*websocket.Conn]string), // connected clients (websocket -> document id)
		connectionsPerDocument: make(map[string]uint),
	}
}

// checkOrigin allows websocket upgrades from the configured CORS origins and from clients
// that send no Origin header (native apps). Without configured origins every origin is allowed.
func checkOrigin(allowedOrigins []string) func(r *http.Request) bool {
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" || len(allowedOrigins) == 0 {
			return true
		}
		for _, allowed := range allowedOrigins {
			if allowed == "*" || strings.EqualFold(allowed, origin) {
				return true
			}
		}
		return false
	}
}

func (wcm *WebsocketConnectionManager) IsClientConnected(documentId string) bool {
	wcm.lock.RLock()
	defer wcm.lock.RUnlock()
	return wcm.connectionsPerDocument[documentId] > 0
}

// handle new websocket connections
func (wcm *WebsocketConnectionManager) HandleNewConnection(c echo.Context, documentId string) (err error) {
	if UserFromContext(c) == nil {
		return echo.ErrUnauthorized
	}
	d := wcm.treeManager.GetDocument(documentId)
	if d == nil {
		return echo.ErrNotFound
	}

	client, err := wcm.upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}

	// Make sure we Close the connection when the function returns
	defer wcm.disconnectClient(client)

	wcm.lock.Lock()
	// Register our new client
	wcm.clients[client] = documentId
	wcm.connectionsPerDocument[documentId] = wcm.connectionsPerDocument[documentId] + 1
	wcm.lock.Unlock()

	err = wcm.onNewClient(client, d)
	if err != nil {
		return err
	}

	for {
		request, err := wcm.parseRequestBody(client)
		if err != nil {
			log.Printf("%v: error: %v", client.RemoteAddr(), err)
			break
		}

		switch r := request.(type) {
		case SyncRequest:
			if err = wcm.handleSyncRequest(client, r); err != nil {
				log.Printf("%v: error: %v", client.RemoteAddr(), err)
			}
		default:
			log.Printf("%v: error: invalid message type: %v", client.RemoteAddr(), request)
		}
	}

	return nil
}

type SocketEntityBase struct {
	Type       string `json:"type" xml:"type" form:"type" query:"type"`
	RequestId  string `json:"requestId" xml:"requestId" form:"requestId" query:"requestId"`
	DocumentId string `json:"documentId" xml:"documentId" form:"documentId" query:"documentId"`
}

func (wcm *WebsocketConnectionManager) parseRequestBody(
	client *websocket.Conn,
) (request interface{}, err error) {
	_, data, err := client.ReadMessage()
	if err != nil {
		return nil, err
	}

	var baseRequest SocketEntityBase
	if err = json.Unmarshal(data, &baseRequest); err != nil {
		return nil, err
	}
	switch baseRequest.Type {
	case TypeSyncRequest:
		var syncRequest SyncRequest
		if err = json.Unmarshal(data, &syncRequest); err != nil {
			return nil, err
		}
		return syncRequest, nil
	}
	return nil, nil
}

func (wcm *WebsocketConnectionManager) handleSyncRequest(client *websocket.Conn, request SyncRequest) (err error) {
	fmt.Printf("%v: %s\n", client.RemoteAddr(), request)
	err = wcm.onSyncRequest(client, request)
	return err
}

// disconnects a client
func (wcm *WebsocketConnectionManager) disconnectClient(conn *websocket.Conn) {
	err := conn.Close()
	if err != nil {
		log.Printf("%v: error closing websocket connection: %v", conn.RemoteAddr(), err)
	}

	wcm.lock.Lock()
	documentId := wcm.clients[conn]

	connectedClientsAfterDisconnect := wcm.connectionsPerDocument[documentId] - 1

	wcm.connectionsPerDocument[documentId] = connectedClientsAfterDisconnect
	wcm.onClientDisconnected(conn, documentId, connectedClientsAfterDisconnect)
	delete(wcm.clients, conn)

	wcm.lock.Unlock()
}

func (wcm *WebsocketConnectionManager) SetOnNewClientListener(f func(client *websocket.Conn, document *Document) error) {
	wcm.onNewClient = f
}

func (wcm *WebsocketConnectionManager) SetOnSyncRequestMessageListener(f func(client *websocket.Conn, request SyncRequest) error) {
	wcm.onSyncRequest = f
}

func (wcm *WebsocketConnectionManager) SetOnClientDisconnectedListener(f func(client *websocket.Conn, documentId string, remainingConnections uint)) {
	wcm.onClientDisconnected = f
}
