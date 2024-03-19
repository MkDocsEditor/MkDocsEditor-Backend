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
)

type WebsockerConnectionManager struct {
	treeManager *TreeManager

	upgrader websocket.Upgrader
	lock     mutexSync.RWMutex
	// connected clients (websocket -> document id)
	clients                map[*websocket.Conn]string
	connectionsPerDocument map[string]uint

	onNewClient          func(client *websocket.Conn, document *Document) error
	onIncomingMessage    func(client *websocket.Conn, request EditRequest) error
	onClientDisconnected func(client *websocket.Conn)
}

func NewWebsocketConnectionManager(
	treeManager *TreeManager,
	onNewClient func(client *websocket.Conn, document *Document) error,
	onIncomingMessage func(client *websocket.Conn, request EditRequest) error,
	onClientDisconnected func(client *websocket.Conn),
) *WebsockerConnectionManager {
	return &WebsockerConnectionManager{
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

		onNewClient:          onNewClient,
		onIncomingMessage:    onIncomingMessage,
		onClientDisconnected: onClientDisconnected,
	}
}

func (wcm *WebsockerConnectionManager) IsClientConnected(documentId string) bool {
	wcm.lock.RLock()
	defer wcm.lock.RUnlock()
	return wcm.connectionsPerDocument[documentId] > 0
}

// handle new websocket connections
func (wcm *WebsockerConnectionManager) handleNewConnection(c echo.Context, documentId string) (err error) {
	d := wcm.treeManager.GetDocument(documentId)
	if d == nil {
		return echo.ErrNotFound
	}

	client, err := wcm.upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}

	// Make sure we close the connection when the function returns
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
		// Read incoming edit requests
		var editRequest EditRequest
		// Read in a new message as JSON and map it to a Message object
		err := client.ReadJSON(&editRequest)
		if err != nil {
			log.Printf("%v: error: %v", client.RemoteAddr(), err)
			break
		}

		// Send the newly received message to the broadcast channel
		err = wcm.handleIncomingMessage(client, editRequest)
		if err != nil {
			log.Printf("%v: error: %v", client.RemoteAddr(), err)
			break
		}

		wcm.saveCurrentDocumentContent(documentId)
	}

	return nil
}

// processes incoming messages from connected clients
func (wcm *WebsockerConnectionManager) handleIncomingMessage(client *websocket.Conn, request EditRequest) (err error) {
	fmt.Printf("%v: %s\n", client.RemoteAddr(), request)
	err = wcm.onIncomingMessage(client, request)
	return err
}

// sends an EditRequest to the specified connection
func (wcm *WebsockerConnectionManager) sendToClient(connection *websocket.Conn, editRequest EditRequest) (err error) {
	err = connection.WriteJSON(editRequest)
	if err != nil {
		log.Printf("%v: error writing EditRequest to websocket client: %v", connection.RemoteAddr(), err)
	}
	return err
}

// disconnects a client
func (wcm *WebsockerConnectionManager) disconnectClient(conn *websocket.Conn) {
	err := conn.Close()
	if err != nil {
		log.Printf("%v: error closing websocket connection: %v", conn.RemoteAddr(), err)
	}

	wcm.lock.Lock()
	documentId := wcm.clients[conn]

	connectedClientsAfterDisconnect := wcm.connectionsPerDocument[documentId] - 1

	wcm.connectionsPerDocument[documentId] = connectedClientsAfterDisconnect
	wcm.onClientDisconnected(conn)
	delete(wcm.clients, conn)

	wcm.lock.Unlock()

	if connectedClientsAfterDisconnect <= 0 {
		wcm.saveCurrentDocumentContent(documentId)
	}
}

func (wcm *WebsockerConnectionManager) saveCurrentDocumentContent(documentId string) {
	// TODO: possibly needs locking to avoid writing the same document when multiple clients are connected and triggering edits
	d := wcm.treeManager.GetDocument(documentId)
	if d == nil {
		log.Printf("Unable to write document content for document %s: Document was nil", documentId)
		return
	}

	err := WriteFile(d.Path, []byte(d.Content))
	if err != nil {
		log.Printf("Unable to write modified document content for document %s: %v", documentId, err)
	}
}
