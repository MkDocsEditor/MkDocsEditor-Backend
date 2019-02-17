package backend

import (
	"crypto/md5"
	"encoding/hex"
	"github.com/gorilla/websocket"
	"log"
)

// Manages processing of EditRequests from clients
var (
	// client connection -> server shadow
	serverShadows = make(map[*websocket.Conn]string)
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

func init() {
}

// sets the initial server shadow for a new client connection
func InitClient(conn *websocket.Conn, shadowContent string) {
	serverShadows[conn] = shadowContent
}

// removes the shadow for the given client
func RemoveClient(conn *websocket.Conn) {
	delete(serverShadows, conn)
}

// handles incoming edit requests from the client
func HandleEditRequest(clientConnection *websocket.Conn, editRequest EditRequest) (patches string, err error) {
	// check if the server shadow matches the client shadow before the patch has been applied
	checksum := GetMD5Hash(serverShadows[clientConnection])
	if checksum == editRequest.ShadowChecksum {
		// if so, patch the server shadow
		patchedServerShadow, err2 := ApplyPatch(serverShadows[clientConnection], editRequest.Patches)
		err = err2
		serverShadows[clientConnection] = patchedServerShadow
	} else {
		// TODO: return error and disconnect client (resync necessary)
		return "", err
	}

	// then patch the server document version
	d := GetDocument(editRequest.DocumentId)
	patchedText, err := ApplyPatch(d.Content, editRequest.Patches)
	if err != nil {
		// TODO: if fuzzy patch fails make a diff of serverShadow and current server version
		log.Fatal(err)
		return "", err
	}
	d.Content = patchedText

	// take a diff between the server document version and the server shadow
	patches, err = CreatePatch(d.Content, serverShadows[clientConnection])
	if err != nil {
		log.Fatal(err)
		return "", err
	}

	// copy the server version to the server shadow
	serverShadows[clientConnection] = d.Content

	// send the diff to all clients
	return patches, err
}

func GetMD5Hash(text string) string {
	hasher := md5.New()
	hasher.Write([]byte(text))
	return hex.EncodeToString(hasher.Sum(nil))
}
