package backend

import (
	"errors"
	"github.com/gorilla/websocket"
	"log"
	"strconv"
)

// Manages processing of EditRequests from clients
var (
	// client connection -> server shadow
	ServerShadows = make(map[*websocket.Conn]string)
)

type (
	InitialContentRequest struct {
		Type       string `json:"type" xml:"type" form:"type" query:"type"`
		RequestId  string `json:"requestId" xml:"requestId" form:"requestId" query:"requestId"`
		DocumentId string `json:"documentId" xml:"documentId" form:"documentId" query:"documentId"`
		Content    string `json:"content" xml:"content" form:"content" query:"content"`
	}

	EditRequest struct {
		Type           string `json:"type" xml:"type" form:"type" query:"type"`
		RequestId      string `json:"requestId" xml:"requestId" form:"requestId" query:"requestId"`
		DocumentId     string `json:"documentId" xml:"documentId" form:"documentId" query:"documentId"`
		Patches        string `json:"patches" xml:"patches" form:"patches" query:"patches"`
		ShadowChecksum string `json:"shadowChecksum" xml:"shadowChecksum" form:"shadowChecksum" query:"shadowChecksum"`
	}
)

// sets the initial server shadow for a new client connection
func initClient(conn *websocket.Conn, shadowContent string) {
	ServerShadows[conn] = shadowContent
}

// removes the shadow for the given client
func removeClient(conn *websocket.Conn) {
	delete(ServerShadows, conn)
}

// handles incoming edit requests from the client
func handleEditRequest(client *websocket.Conn, editRequest EditRequest) (err error) {
	// check if the server shadow matches the client shadow before the patch has been applied
	checksum := calculateChecksum(ServerShadows[client])
	if checksum != editRequest.ShadowChecksum {
		return errors.New("unrecoverable: shadow out of sync")
	}

	documentId := editRequest.DocumentId

	// patch the server shadow
	ServerShadows[client], err = ApplyPatch(ServerShadows[client], editRequest.Patches)

	// then patch the server document version
	d := GetDocument(editRequest.DocumentId)
	patchedText, err := ApplyPatch(d.Content, editRequest.Patches)
	if err != nil {
		// if fuzzy patch fails, drop client changes
		log.Printf("fuzzy patch failed")
		// reset err variable as we can recover from this error
		err = nil
	} else {
		d.Content = patchedText
	}

	if err != nil {
		log.Printf("error handling EditRequest, resending InitialText to %v", client.RemoteAddr())
		// force resync
		d := GetDocument(documentId)
		err = sendInitialTextResponse(client, d)
		return err
	}

	err = sendEditRequestResponse(client, documentId)
	if err != nil {
		log.Printf("error sending response: %v", err)
		return err
	}

	return err
}

func sendInitialTextResponse(client *websocket.Conn, document *Document) (err error) {
	// set initial state in backend
	initClient(client, document.Content)

	initialContentRequest := InitialContentRequest{
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

func sendEditRequestResponse(inducingConnection *websocket.Conn, documentId string) (err error) {
	d := GetDocument(documentId)

	shadow := ServerShadows[inducingConnection]
	shadowChecksum := calculateChecksum(shadow)

	patches, err := CreatePatch(shadow, d.Content)
	if err != nil {
		log.Fatal(err)
		return err
	}

	// if this is the connection that caused the change
	// if connection == inducingConnection {
	// copy the server version to the server shadow
	ServerShadows[inducingConnection] = d.Content
	// }

	if len(patches) <= 0 {
		return
	}

	serverEditRequest := EditRequest{
		Type:           TypeEditRequest,
		RequestId:      "",
		DocumentId:     documentId,
		Patches:        patches,
		ShadowChecksum: shadowChecksum,
	}

	err = sendToClient(inducingConnection, serverEditRequest)

	return err
}

func calculateChecksum(text string) string {
	return strconv.Itoa(len([]rune(text)))
	//return text
	// TODO: this md5 is sometimes not the same as in kotlin...
	//hash := md5.Sum([]byte(text))
	//return hex.EncodeToString(hash[:])
}
