package backend

import (
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"log"
	"net/http"
	mutexSync "sync"
)

const (
	TypeInitialContent = "initial-content"
	TypeEditRequest    = "edit-request"
	TypeSyncRequest    = "sync-request"
)

type WebsocketConnectionManager struct {
	treeManager *TreeManager

	upgrader websocket.Upgrader
	lock     mutexSync.RWMutex
	// connected clients (websocket -> document id)
	clients                map[*websocket.Conn]string
	connectionsPerDocument map[string]uint

	onNewClient          func(client *websocket.Conn, document *Document) error
	onIncomingMessage    func(client *websocket.Conn, request EditRequest) error
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
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		lock:                   mutexSync.RWMutex{},
		clients:                make(map[*websocket.Conn]string), // connected clients (websocket -> document id)
		connectionsPerDocument: make(map[string]uint),
	}
}

func (wcm *WebsocketConnectionManager) IsClientConnected(documentId string) bool {
	wcm.lock.RLock()
	defer wcm.lock.RUnlock()
	return wcm.connectionsPerDocument[documentId] > 0
}

// handle new websocket connections
func (wcm *WebsocketConnectionManager) HandleNewConnection(c echo.Context, documentId string) (err error) {
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

		switch request.(type) {
		//case InitialContentRequest:
		case EditRequest:
			// Send the newly received message to the broadcast channel
			err = wcm.handleIncomingMessage(client, request.(EditRequest))
			if err != nil {
				log.Printf("%v: error: %v", client.RemoteAddr(), err)
				break
			}
		case SyncRequest:
			err = wcm.handleSyncRequest(client, request.(SyncRequest))
			if err != nil {
				log.Printf("%v: error: %v", client.RemoteAddr(), err)
				break
			}
		default:
			log.Printf("%v: error: invalid message type: %v", client.RemoteAddr(), request)
			break
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
	var baseRequest SocketEntityBase

	err = client.ReadJSON(&baseRequest)
	if err != nil {
		return nil, err
	}
	switch baseRequest.Type {
	case TypeInitialContent:
		var initialContentRequest InitialContentRequest
		err = client.ReadJSON(&initialContentRequest)
		if err != nil {
			return nil, err
		}
		return initialContentRequest, nil
	case TypeEditRequest:
		var editRequest EditRequest
		err = client.ReadJSON(&editRequest)
		if err != nil {
			return nil, err
		}
		return editRequest, nil
	case TypeSyncRequest:
		var syncRequest SyncRequest
		err = client.ReadJSON(&syncRequest)
		if err != nil {
			return nil, err
		}
		return syncRequest, nil
	}
	return nil, nil
}

// processes incoming messages from connected clients
func (wcm *WebsocketConnectionManager) handleIncomingMessage(client *websocket.Conn, request EditRequest) (err error) {
	fmt.Printf("%v: %s\n", client.RemoteAddr(), request)
	err = wcm.onIncomingMessage(client, request)
	return err
}

// sends an EditRequest to the specified connection
func (wcm *WebsocketConnectionManager) sendToClient(connection *websocket.Conn, editRequest EditRequest) (err error) {
	err = connection.WriteJSON(editRequest)
	if err != nil {
		log.Printf("%v: error writing EditRequest to websocket client: %v", connection.RemoteAddr(), err)
	}
	return err
}

func (wcm *WebsocketConnectionManager) handleSyncRequest(client *websocket.Conn, request SyncRequest) (err error) {
	fmt.Printf("%v: %s\n", client.RemoteAddr(), request)
	err = wcm.onSyncRequest(client, request)
	return err
}

// sends an SyncRequest to the specified connection
func (wcm *WebsocketConnectionManager) syncStateToClient(connection *websocket.Conn, syncStateRequest SyncRequest) (err error) {
	err = connection.WriteJSON(syncStateRequest)
	if err != nil {
		log.Printf("%v: error writing EditRequest to websocket client: %v", connection.RemoteAddr(), err)
	}
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

func (wcm *WebsocketConnectionManager) SetOnIncomingEditRequestMessageListener(f func(client *websocket.Conn, request EditRequest) error) {
	wcm.onIncomingMessage = f
}

func (wcm *WebsocketConnectionManager) SetOnSyncRequestMessageListener(f func(client *websocket.Conn, request SyncRequest) error) {
	wcm.onSyncRequest = f
}

func (wcm *WebsocketConnectionManager) SetOnClientDisconnectedListener(f func(client *websocket.Conn, documentId string, remainingConnections uint)) {
	wcm.onClientDisconnected = f
}
