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
}

func handleNewConnections(c echo.Context) (err error) {
	documentId := c.Param(urlParamId)

	d := backend.GetDocument(documentId)
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
		var editRequest backend.EditRequest
		// Read in a new message as JSON and map it to a Message object
		err := client.ReadJSON(&editRequest)
		if err != nil {
			log.Printf("error: %v", err)
			return err
		}

		// Send the newly received message to the broadcast channel
		err = handleIncomingMessage(client, editRequest)
		if err != nil {
			log.Printf("error: %v", err)
			return err
		}
	}
}

func sendInitialTextResponse(client *websocket.Conn, document *backend.Document) (err error) {
	// set initial state in backend
	backend.InitClient(client, document.Content)

	initialContentRequest := backend.InitialContentRequest{
		Type:       TypeInitialContent,
		DocumentId: document.ID,
		RequestId:  "",
		Content:    document.Content,
	}

	// Write current document state to the client
	err = client.WriteJSON(initialContentRequest)
	if err != nil {
		log.Printf("error writing initial content response: %v", err)
		return err
	}

	return
}

// processes incoming messages from connected clients
func handleIncomingMessage(client *websocket.Conn, request backend.EditRequest) (err error) {
	fmt.Printf("%v: %s\n", client.RemoteAddr(), request)
	documentId := request.DocumentId

	err = backend.HandleEditRequest(client, request)
	if err != nil {
		log.Printf("error handling EditRequest: %v", err)
		log.Printf("resending InitialText to %v", client.RemoteAddr())
		// force resync
		d := backend.GetDocument(documentId)
		sendInitialTextResponse(client, d)
		return err
	}

	err = sendEditRequestResponse(client, documentId)
	if err != nil {
		log.Printf("error sending response: %v", err)
		return err
	}

	return err
}

func sendEditRequestResponse(inducingConnection *websocket.Conn, documentId string) (err error) {
	d := backend.GetDocument(documentId)

	shadow := backend.ServerShadows[inducingConnection]
	shadowChecksum := backend.GetMD5Hash(shadow)

	patches, err := backend.CreatePatch(shadow, d.Content)
	if err != nil {
		log.Fatal(err)
		return err
	}

	// if this is the connection that caused the change
	// if connection == inducingConnection {
	// copy the server version to the server shadow
	backend.ServerShadows[inducingConnection] = d.Content
	// }

	if len(patches) <= 0 {
		return
	}

	serverEditRequest := backend.EditRequest{
		Type:           TypeEditRequest,
		RequestId:      "",
		DocumentId:     documentId,
		Patches:        patches,
		ShadowChecksum: shadowChecksum,
	}

	err = sendToClient(inducingConnection, serverEditRequest)

	return err
}

// sends an EditRequest to the specified connection
func sendToClient(connection *websocket.Conn, editRequest backend.EditRequest) (err error) {
	err = connection.WriteJSON(editRequest)
	if err != nil {
		log.Printf("error writing EditRequest to websocket client: %v", err)
	}
	return err
}

// disconnects a client
func disconnectClient(conn *websocket.Conn) {
	err := conn.Close()
	if err != nil {
		log.Printf("error closing websocket connection: %v", err)
	}

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

		err := backend.WriteFile(d.Path, []byte(d.Content))
		if err != nil {
			log.Printf("error writing edited file to disk: %v", err)
		}
	}
}
