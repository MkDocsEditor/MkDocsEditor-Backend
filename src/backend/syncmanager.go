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

func init() {
}

// sets the initial server shadow for a new client connection
func InitClient(conn *websocket.Conn, shadowContent string) {
	ServerShadows[conn] = shadowContent
}

// removes the shadow for the given client
func RemoveClient(conn *websocket.Conn) {
	delete(ServerShadows, conn)
}

// handles incoming edit requests from the client
func HandleEditRequest(clientConnection *websocket.Conn, editRequest EditRequest) (err error) {
	// check if the server shadow matches the client shadow before the patch has been applied
	checksum := GetMD5Hash(ServerShadows[clientConnection])
	if checksum != editRequest.ShadowChecksum {
		// TODO: this happens more often than it should, why?
		log.Printf("shadows out of sync!")
		log.Printf("server Shadow: %s", ServerShadows[clientConnection])
		log.Printf("server Shadow chksum: %s Client Shadow Checksum: %s", checksum, editRequest.ShadowChecksum)

		return errors.New("unrecoverable: shadow out of sync")
	}

	// patch the server shadow
	ServerShadows[clientConnection], err = ApplyPatch(ServerShadows[clientConnection], editRequest.Patches)

	// then patch the server document version
	d := GetDocument(editRequest.DocumentId)
	patchedText, err := ApplyPatch(d.Content, editRequest.Patches)
	if err != nil {
		// TODO: if fuzzy patch fails make a diff of serverShadow and current server version
		log.Fatal(err)
		return err
	}
	d.Content = patchedText

	return err
}

func GetMD5Hash(text string) string {
	return strconv.Itoa(len(text))
	// TODO: this md5 is sometimes not the same as in kotlin...
	//hash := md5.Sum([]byte(text))
	//return hex.EncodeToString(hash[:])
}
