package frontend

import (
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo"
	"log"
	"mkdocsrest/backend"
	"sync"
)

type (
	EditRequest struct {
		RequestId  string `json:"requestId" xml:"requestId" form:"requestId" query:"requestId"`
		DocumentId string `json:"documentId" xml:"documentId" form:"documentId" query:"documentId"`
		Patches    string `json:"patches" xml:"patches" form:"patches" query:"patches"`
	}
)

var (
	upgrader = websocket.Upgrader{}

	lock = sync.RWMutex{}

	clients                = make(map[*websocket.Conn]string) // connected clients (websocket -> document id)
	connectionsPerDocument = make(map[string]uint)
	incomingEditRequests   = make(chan EditRequest) // incoming messages from clients
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
	defer client.Close()

	lock.Lock()

	// Register our new client
	clients[client] = id
	connectionsPerDocument[id] = connectionsPerDocument[id] + 1

	lock.Unlock()

	// Write current document state to the client
	err = client.WriteMessage(websocket.TextMessage, []byte(d.Content))
	if err != nil {
		c.Logger().Error(err)
		disconnectClient(client)
	}

	for {
		// Read incoming edit requests
		var editRequest EditRequest
		// Read in a new message as JSON and map it to a Message object
		err := client.ReadJSON(&editRequest)
		if err != nil {
			log.Printf("error: %v", err)
			disconnectClient(client)
			break
		}
		fmt.Printf("%s\n", editRequest)

		// Send the newly received message to the broadcast channel
		incomingEditRequests <- editRequest
	}

	return err
}

// processes incoming messages from connected clients
func handleIncomingMessages() {
	for {
		// Grab the next message from the broadcast channel
		editRequest := <-incomingEditRequests

		d := backend.GetDocument(editRequest.DocumentId)
		patchedText, err := backend.ApplyPatch(d, editRequest.Patches)
		if err != nil {
			log.Fatal(err)
		}
		d.Content = patchedText

		// Send it out to every client that is currently connected
		for client, documentId := range clients {
			// skip clients that have other documents open
			if (documentId) != editRequest.DocumentId {
				continue
			}

			//err := client.WriteMessage(websocket.TextMessage, []byte(d.Content))
			err := client.WriteJSON(editRequest)
			if err != nil {
				log.Printf("error: %v", err)
				disconnectClient(client)
			}
		}
	}
}

// disconnects a client
func disconnectClient(conn *websocket.Conn) {
	conn.Close()

	lock.Lock()
	documentId := clients[conn]

	connectedClientsAfterDisconnect := connectionsPerDocument[documentId] - 1

	connectionsPerDocument[documentId] = connectedClientsAfterDisconnect
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
