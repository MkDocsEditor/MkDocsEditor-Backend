package backend

import (
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"log"
	mutexSync "sync"
)

const (
	TypeInitialContent = "initial-content"
	TypeEditRequest    = "edit-request"

	ApiRequestDeleteSection  = "DeleteSection"
	ApiRequestDeleteDocument = "DeleteDocument"
	ApiRequestDeleteResource = "DeleteResource"
)

var (
	upgrader = websocket.Upgrader{}

	lock = mutexSync.RWMutex{}

	rootClients            = make(map[*websocket.Conn]string) // connected root clients (websocket -> document id)
	documentClients        = make(map[*websocket.Conn]string) // connected document clients (websocket -> document id)
	connectionsPerDocument = make(map[string]uint)
)

type (
	ApiRequest struct {
		Command string `json:"command" xml:"command" form:"command" query:"command"`
		Data    string `json:"data" xml:"data" form:"data" query:"data"`
	}

	DeleteSectionRequest struct {
		sectionId string `json:"sectionId" xml:"sectionId" form:"sectionId" query:"sectionId"`
	}
)

// handle new websocket connections
func onDocumentWsConnection(c echo.Context) (err error) {
	// retrieve the document using the id from URL parameters
	documentId := c.Param(urlParamId)
	d := GetDocument(documentId)
	if d == nil {
		return returnNotFound(c, documentId)
	}

	// upgrade the connection to a Websocket
	client, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return returnError(c, err)
	}

	// Make sure we close the connection when the function returns
	defer disconnectClient(client)

	lock.Lock()
	// Register our new client
	documentClients[client] = documentId
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
			return err
		}

		// Send the newly received message to the broadcast channel
		err = handleIncomingMessage(client, editRequest)
		if err != nil {
			log.Printf("%v: error: %v", client.RemoteAddr(), err)
			return err
		}
	}
}

func onRootWsConnection(c echo.Context) (err error) {
	client, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return returnError(c, err)
	}

	// Make sure we close the connection when the function returns
	defer disconnectClient(client)

	// Register our new client
	rootClients[client] = client.RemoteAddr().String()

	for {
		// Read incoming edit requests
		var apiRequest ApiRequest
		// Read in a new message as JSON and map it to a Message object
		err := client.ReadJSON(&apiRequest)
		if err != nil {
			log.Printf("%v: error: %v", client.RemoteAddr(), err)
			return err
		}

		// Send the newly received message to the broadcast channel
		err = handleApiRequest(client, apiRequest)
		if err != nil {
			log.Printf("%v: error: %v", client.RemoteAddr(), err)
			return err
		}
	}
}

// processes incoming messages from connected documentClients
func handleApiRequest(client *websocket.Conn, request ApiRequest) (err error) {
	fmt.Printf("%v: %s\n", client.RemoteAddr(), request)

	switch request.Command {
	case ApiRequestDeleteSection:
		var requestData DeleteSectionRequest
		json.Unmarshal([]byte(request.Data), &requestData)

		success, err := DeleteItem(requestData.sectionId, TypeSection)
		if err != nil {
			return err
		}

		if !success {
			// TODO: return "not found"
		} else {
			CreateItemTree()
			// TODO: return "success"
		}
	case ApiRequestDeleteDocument:
		// TODO
	case ApiRequestDeleteResource:
		// TODO
	default:
		// TODO: return "unknown command"
	}

	return err
}

// processes incoming messages from connected documentClients
func handleIncomingMessage(client *websocket.Conn, request EditRequest) (err error) {
	fmt.Printf("%v: %s\n", client.RemoteAddr(), request)
	err = handleEditRequest(client, request)
	return err
}

// sends the specified object to the specified websocket client connection
func sendToClient(connection *websocket.Conn, jsonData interface{}) (err error) {
	err = connection.WriteJSON(jsonData)
	if err != nil {
		log.Printf("%v: error writing json data to websocket client: %v", connection.RemoteAddr(), err)
	}
	return err
}

// disconnects a document client
func disconnectClient(client *websocket.Conn) {
	err := client.Close()
	if err != nil {
		log.Printf("%v: error closing websocket connection: %v", client.RemoteAddr(), err)
	}

	lock.Lock()
	documentId, docOk := documentClients[client]
	_, rootOk := rootClients[client]
	if docOk {
		connectedClientsAfterDisconnect := connectionsPerDocument[documentId] - 1

		connectionsPerDocument[documentId] = connectedClientsAfterDisconnect
		removeShadow(client)
		delete(documentClients, client)

		lock.Unlock()

		if connectedClientsAfterDisconnect <= 0 {
			d := GetDocument(documentId)
			if d == nil {
				log.Fatal("Document was nil!")
			}

			err := WriteFile(d.Path, []byte(d.Content))
			if err != nil {
				log.Printf("error writing edited file to disk: %v", err)
			}
		}
	} else if rootOk {
		delete(rootClients, client)
	} else {
		lock.Unlock()
	}

}
