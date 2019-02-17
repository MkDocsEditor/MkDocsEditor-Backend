package frontend

import (
	"MkDocsEditor-Backend/src/backend"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo"
	"log"
	mutexSync "sync"
)

const (
	TypeInitialContent = "initial-content"
	TypeEditRequest    = "edit-request"
)

var (
	upgrader = websocket.Upgrader{}

	lock = mutexSync.RWMutex{}

	clients                = make(map[*websocket.Conn]string) // connected clients (websocket -> document id)
	connectionsPerDocument = make(map[string]uint)
	incomingWsMessages     = make(chan IncomingWebsocketRequest) // incoming messages from clients
)

type (
	IncomingWebsocketRequest struct {
		connection *websocket.Conn
		request    backend.EditRequest
	}
)

func init() {
	go handleIncomingMessages()
}

func handleNewConnections(c echo.Context) (err error) {
	id := c.Param(urlParamId)

	d := backend.GetDocument(id)
	if d == nil {
		return returnNotFound(c, id)
	}

	client, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}

	// Make sure we close the connection when the function returns
	defer disconnectClient(client)

	lock.Lock()

	// Register our new client
	clients[client] = id
	connectionsPerDocument[id] = connectionsPerDocument[id] + 1

	lock.Unlock()

	// set initial state in backend
	backend.InitClient(client, d.Content)

	initialContentRequest := backend.InitialContentRequest{
		Type:       TypeInitialContent,
		DocumentId: id,
		RequestId:  id,
		Content:    d.Content,
	}

	// Write current document state to the client
	err = client.WriteJSON(initialContentRequest)
	if err != nil {
		c.Logger().Error(err)
		disconnectClient(client)
	}

	for {
		// Read incoming edit requests
		var editRequest backend.EditRequest
		// Read in a new message as JSON and map it to a Message object
		err := client.ReadJSON(&editRequest)
		if err != nil {
			log.Printf("error: %v", err)
			disconnectClient(client)
			break
		}
		fmt.Printf("%s\n", editRequest)

		// Send the newly received message to the broadcast channel
		incomingWsMessages <- IncomingWebsocketRequest{
			connection: client,
			request:    editRequest,
		}
	}

	return err
}

// processes incoming messages from connected clients
func handleIncomingMessages() {
	for {
		// Grab the next message from the broadcast channel
		incomingWsMessage := <-incomingWsMessages

		documentId := incomingWsMessage.request.DocumentId

		err := backend.HandleEditRequest(incomingWsMessage.connection, incomingWsMessage.request)
		if err != nil {
			// force resync
			disconnectClient(incomingWsMessage.connection)
			continue
		} else {
			NotifyClientsOfChange(documentId)
		}
	}
}

func NotifyClientsOfChange(documentId string) {
	d := backend.GetDocument(documentId)

	// take a diff between the server document version and the server shadow
	for connection, shadow := range backend.ServerShadows {
		if clients[connection] != d.ID {
			// this client is working on another document
			continue
		}

		patches, err := backend.CreatePatch(shadow, d.Content)
		if err != nil {
			log.Fatal(err)
			continue
		}

		if len(patches) <= 0 {
			continue
		}

		// copy the server version to the server shadow
		backend.ServerShadows[connection] = d.Content

		serverEditRequest := backend.EditRequest{
			Type:           TypeEditRequest,
			RequestId:      "",
			DocumentId:     documentId,
			Patches:        patches,
			ShadowChecksum: "unused",
		}

		sendToClient(connection, serverEditRequest)
	}
}

// sends an EditRequest to the specified connection
func sendToClient(connection *websocket.Conn, editRequest backend.EditRequest) {
	err := connection.WriteJSON(editRequest)
	if err != nil {
		log.Printf("error: %v", err)
		disconnectClient(connection)
	}
}

// disconnects a client
func disconnectClient(conn *websocket.Conn) {
	conn.Close()

	lock.Lock()
	documentId := clients[conn]

	connectedClientsAfterDisconnect := connectionsPerDocument[documentId] - 1

	connectionsPerDocument[documentId] = connectedClientsAfterDisconnect
	backend.RemoveClient(conn)
	delete(clients, conn)

	lock.Unlock()

	if connectedClientsAfterDisconnect <= 0 {
		d := backend.GetDocument(documentId)
		if d == nil {
			log.Fatal("Document was nil!")
		}

		backend.WriteFile(d.Path, []byte(d.Content))
	}
}
