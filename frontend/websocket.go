package frontend

import (
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo"
	"mkdocsrest/backend"
	"log"
)

type (
	EditRequest struct {
		DocumentId string `json:"documentId" xml:"documentId" form:"documentId" query:"documentId"`
		Patches    string `json:"patches" xml:"patches" form:"patches" query:"patches"`
	}
)

var (
	upgrader = websocket.Upgrader{}

	clients              = make(map[*websocket.Conn]string) // connected clients (websocket -> document id)
	incomingEditRequests = make(chan EditRequest)           // incoming messages from clients
)

func init() {
	go handleIncomingMessages()
}

func handleDocumentWebsocketConnections(c echo.Context) (err error) {
	id := c.Param(urlParamId)

	d := backend.GetDocument(id)
	if d == nil {
		return returnNotFound(c, id)
	}

	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}

	// Make sure we close the connection when the function returns
	defer ws.Close()

	// Register our new client
	clients[ws] = id

	// Write current document state to the client
	err = ws.WriteMessage(websocket.TextMessage, []byte(backend.GetDocument(id).Content))
	if err != nil {
		c.Logger().Error(err)
		delete(clients, ws)
	}

	for {
		// Read incoming edit requests
		var editRequest EditRequest
		// Read in a new message as JSON and map it to a Message object
		err := ws.ReadJSON(&editRequest)
		if err != nil {
			log.Printf("error: %v", err)
			delete(clients, ws)
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

		// Send it out to every client that is currently connected
		for client := range clients {
			err := client.WriteMessage(websocket.TextMessage, []byte(patchedText))
			//err := client.WriteJSON(editRequest)
			if err != nil {
				log.Printf("error: %v", err)
				client.Close()
				delete(clients, client)
			}
		}
	}
}
