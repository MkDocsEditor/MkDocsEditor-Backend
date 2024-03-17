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

var (
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	lock = mutexSync.RWMutex{}

	clients                = make(map[*websocket.Conn]string) // connected clients (websocket -> document id)
	connectionsPerDocument = make(map[string]uint)
)

func IsClientConnected(documentId string) bool {
	lock.RLock()
	defer lock.RUnlock()
	return connectionsPerDocument[documentId] > 0
}

// handle new websocket connections
func handleNewConnection(c echo.Context) (err error) {
	documentId := c.Param(urlParamId)

	d := GetDocument(documentId)
	if d == nil {
		return returnNotFound(c, documentId)
	}

	client, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return returnError(c, err)
	}

	// Make sure we close the connection when the function returns
	defer disconnectClient(client)

	lock.Lock()
	// Register our new client
	clients[client] = documentId
	connectionsPerDocument[documentId] = connectionsPerDocument[documentId] + 1
	lock.Unlock()

	err = sendInitialTextResponse(client, d)
	if err != nil {
		return returnError(c, err)
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
		err = handleIncomingMessage(client, editRequest)
		if err != nil {
			log.Printf("%v: error: %v", client.RemoteAddr(), err)
			break
		}

		saveCurrentDocumentContent(documentId)
	}

	return nil
}

// processes incoming messages from connected clients
func handleIncomingMessage(client *websocket.Conn, request EditRequest) (err error) {
	fmt.Printf("%v: %s\n", client.RemoteAddr(), request)
	err = handleEditRequest(client, request)
	return err
}

// sends an EditRequest to the specified connection
func sendToClient(connection *websocket.Conn, editRequest EditRequest) (err error) {
	err = connection.WriteJSON(editRequest)
	if err != nil {
		log.Printf("%v: error writing EditRequest to websocket client: %v", connection.RemoteAddr(), err)
	}
	return err
}

// disconnects a client
func disconnectClient(conn *websocket.Conn) {
	err := conn.Close()
	if err != nil {
		log.Printf("%v: error closing websocket connection: %v", conn.RemoteAddr(), err)
	}

	lock.Lock()
	documentId := clients[conn]

	connectedClientsAfterDisconnect := connectionsPerDocument[documentId] - 1

	connectionsPerDocument[documentId] = connectedClientsAfterDisconnect
	removeClient(conn)
	delete(clients, conn)

	lock.Unlock()

	if connectedClientsAfterDisconnect <= 0 {
		saveCurrentDocumentContent(documentId)
	}
}

func saveCurrentDocumentContent(documentId string) {
	// TODO: possibly needs locking to avoid writing the same document when multiple clients are connected and triggering edits
	d := GetDocument(documentId)
	if d == nil {
		log.Printf("Unable to write document content for document %s: Document was nil", documentId)
		return
	}

	err := WriteFile(d.Path, []byte(d.Content))
	if err != nil {
		log.Printf("Unable to write modified document content for document %s: %v", documentId, err)
	}
}
