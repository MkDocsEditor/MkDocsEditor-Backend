package backend

import (
	"crypto/md5"
	"fmt"
	"github.com/gorilla/websocket"
	"golang.org/x/text/encoding/unicode"
	"log"
	"strings"
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
	documentId := editRequest.DocumentId

	// check if the server shadow matches the client shadow before the patch has been applied
	checksum := calculateChecksum(ServerShadows[client])
	if checksum != editRequest.ShadowChecksum {
		log.Printf("%v: shadow out of sync (got %v but expected %v", client.RemoteAddr(), editRequest.ShadowChecksum, checksum)
		err = sendInitialTextResponse(client, GetDocument(documentId)) // force resync
		if err != nil {
			log.Printf("%v: unable to resync with client: %v", client.RemoteAddr(), err)
			return err
		}
		return
	}

	// patch the server shadow
	ServerShadows[client], err = ApplyPatch(ServerShadows[client], editRequest.Patches)

	// then patch the server document version
	d := GetDocument(documentId)
	patchedText, err := ApplyPatch(d.Content, editRequest.Patches)
	if err != nil {
		// if fuzzy patch fails, drop client changes
		log.Printf("%v: fuzzy patch failed: %v", client.RemoteAddr(), err)
		// reset err variable as we can recover from this error
		err = nil
	} else {
		d.Content = patchedText
	}

	err = sendEditRequestResponse(client, documentId)
	if err != nil {
		log.Printf("%v: error sending response: %v", client.RemoteAddr(), err)
		return err
	}

	return err
}

// send the full document text to a client
func sendInitialTextResponse(client *websocket.Conn, document *Document) (err error) {
	// set initial state in backend
	initClient(client, document.Content)

	// Write current document state to the client
	err = client.WriteJSON(InitialContentRequest{
		Type:       TypeInitialContent,
		DocumentId: document.ID,
		RequestId:  "",
		Content:    document.Content,
	})
	if err != nil {
		log.Printf("%v: error writing initial content response: %v", client.RemoteAddr(), err)
		return err
	}

	return
}

// responds to a client with the changes from the server site document version
func sendEditRequestResponse(client *websocket.Conn, documentId string) (err error) {
	d := GetDocument(documentId)

	shadow := ServerShadows[client]
	shadowChecksum := calculateChecksum(shadow)

	patches, err := CreatePatch(shadow, d.Content)
	if err != nil {
		log.Fatal(err)
		return err
	}
	ServerShadows[client] = d.Content

	// we can skip this if there are no changes that need to be passed to the client
	if len(patches) <= 0 {
		return
	}

	return sendToClient(client,
		EditRequest{
			Type:           TypeEditRequest,
			RequestId:      "",
			DocumentId:     documentId,
			Patches:        patches,
			ShadowChecksum: shadowChecksum,
		})
}

// calculates a checksum for a given text using the MD5 hashing algorithm.
//
// important notes for the implementation of this method:
// - the text that is hashed must be encoded using UTF-16LE without BOM
//   this will ensure the bytes are the same on all clients
// - the checksum string must include leading zeros
// - all characters are lowercase
func calculateChecksum(text string) string {
	encoder := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewEncoder()
	utf16, err := encoder.String(text)
	if err != nil {
		log.Printf("Error encoding String to UTF-16: %v", err)
	}

	hash := md5.Sum([]byte(utf16))
	checksum := fmt.Sprintf("%02x", hash[:])
	return strings.ToLower(checksum)
}
