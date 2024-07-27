package backend

import (
	"crypto/md5"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"golang.org/x/text/encoding/unicode"
	"log"
	"strings"
	mutexSync "sync"
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

type SyncManager interface {
	IsItemBeingEditedRecursive(s *Section) (err error)
}

// DSSyncManager manages processing of EditRequests from clients
type DSSyncManager struct {
	treeManager                *TreeManager
	websocketConnectionManager *WebsocketConnectionManager

	// ServerShadows client connection -> server shadow
	ServerShadows map[*websocket.Conn]string

	// lock for synchronizing the tree to the disk
	lock mutexSync.RWMutex
}

func NewSyncManager(
	treeManager *TreeManager,
) *DSSyncManager {
	syncManager := &DSSyncManager{
		treeManager:   treeManager,
		ServerShadows: make(map[*websocket.Conn]string),
	}

	return syncManager
}

func (sm *DSSyncManager) IsItemBeingEditedRecursive(s *Section) (err error) {
	for _, doc := range *s.Documents {
		if sm.websocketConnectionManager.IsClientConnected(doc.ID) {
			return errors.New("a document within this section is currently being edited by another user")
		}
	}

	for _, subsection := range *s.Subsections {
		err = sm.IsItemBeingEditedRecursive(subsection)
		if err != nil {
			return err
		}
	}

	return nil
}

// sets the initial server shadow for a new client connection
func (sm *DSSyncManager) initClient(conn *websocket.Conn, shadowContent string) {
	sm.ServerShadows[conn] = shadowContent
}

// removes the shadow for the given client
func (sm *DSSyncManager) removeClient(conn *websocket.Conn) {
	delete(sm.ServerShadows, conn)
}

// handles incoming edit requests from the client
func (sm *DSSyncManager) handleEditRequest(client *websocket.Conn, editRequest EditRequest) (err error) {
	documentId := editRequest.DocumentId

	// check if the server shadow matches the client shadow before the patch has been applied
	checksum := sm.calculateChecksum(sm.ServerShadows[client])
	if checksum != editRequest.ShadowChecksum {
		log.Printf("%v: shadow out of sync (got %v but expected %v", client.RemoteAddr(), editRequest.ShadowChecksum, checksum)
		err = sm.sendInitialTextResponse(client, sm.treeManager.GetDocument(documentId)) // force resync
		if err != nil {
			log.Printf("%v: unable to resync with client: %v", client.RemoteAddr(), err)
			return err
		}
		return
	}

	// patch the server shadow
	sm.ServerShadows[client], err = ApplyPatch(sm.ServerShadows[client], editRequest.Patches)

	// then patch the server document version
	d := sm.treeManager.GetDocument(documentId)
	patchedText, err := ApplyPatch(d.Content, editRequest.Patches)
	if err != nil {
		// if fuzzy patch fails, drop client changes
		log.Printf("%v: fuzzy patch failed: %v", client.RemoteAddr(), err)
		// reset err variable as we can recover from this error
		err = nil
	} else {
		if d.Content != patchedText {
			defer sm.saveCurrentDocumentContent(documentId)
		}
		d.Content = patchedText
	}

	err = sm.sendEditRequestResponse(client, documentId)
	if err != nil {
		log.Printf("%v: error sending response: %v", client.RemoteAddr(), err)
		return err
	}

	return err
}

// send the full document text to a client
func (sm *DSSyncManager) sendInitialTextResponse(client *websocket.Conn, document *Document) (err error) {
	// set initial state in backend
	sm.initClient(client, document.Content)

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
func (sm *DSSyncManager) sendEditRequestResponse(client *websocket.Conn, documentId string) (err error) {
	d := sm.treeManager.GetDocument(documentId)

	shadow := sm.ServerShadows[client]
	shadowChecksum := sm.calculateChecksum(shadow)

	patches, err := CreatePatch(shadow, d.Content)
	if err != nil {
		log.Printf("Error creating patch: %v", err)
		return err
	}
	sm.ServerShadows[client] = d.Content

	// we can skip this if there are no changes that need to be passed to the client
	if len(patches) <= 0 {
		return
	}

	return sm.websocketConnectionManager.sendToClient(client,
		EditRequest{
			Type:           TypeEditRequest,
			RequestId:      "",
			DocumentId:     documentId,
			Patches:        patches,
			ShadowChecksum: shadowChecksum,
		})
}

// calculateChecksum calculates a checksum for a given text using the MD5 hashing algorithm.
//
// important notes for the implementation of this method:
//   - the text that is hashed must be encoded using UTF-16LE without BOM
//     this will ensure the bytes are the same on all clients
//   - the checksum string must include leading zeros
//   - all characters are lowercase
func (sm *DSSyncManager) calculateChecksum(text string) string {
	encoder := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewEncoder()
	utf16, err := encoder.String(text)
	if err != nil {
		log.Printf("Error encoding String to UTF-16: %v", err)
	}

	hash := md5.Sum([]byte(utf16))
	checksum := fmt.Sprintf("%02x", hash[:])
	return strings.ToLower(checksum)
}

func (sm *DSSyncManager) saveCurrentDocumentContent(documentId string) {
	sm.lock.RLock()
	defer sm.lock.RUnlock()

	log.Printf("Synchronizing document '%s' synchronized to disk", documentId)

	d := sm.treeManager.GetDocument(documentId)
	if d == nil {
		log.Printf("Unable to write document content for document %s: Document was nil", documentId)
		return
	}

	err := WriteFile(d.Path, []byte(d.Content))
	if err != nil {
		log.Printf("Unable to write modified document content for document %s: %v", documentId, err)
	}

	log.Printf("Document '%s' synchronized to disk successfully", documentId)
}

func (sm *DSSyncManager) SetWebsocketConnectionManager(manager *WebsocketConnectionManager) {
	sm.websocketConnectionManager = manager

	sm.websocketConnectionManager.SetOnNewClientListener(func(client *websocket.Conn, document *Document) error {
		fmt.Println("New client connected", client)
		return sm.sendInitialTextResponse(client, document)
	})
	sm.websocketConnectionManager.SetOnIncomingEditRequestMessageListener(func(client *websocket.Conn, request EditRequest) error {
		fmt.Println("Incoming message from client", client)
		return sm.handleEditRequest(client, request)
	})
	sm.websocketConnectionManager.SetOnClientDisconnectedListener(func(client *websocket.Conn, documentId string, remainingConnections uint) {
		fmt.Println("Client disconnected", client)
		sm.removeClient(client)
		if remainingConnections <= 0 {
			sm.saveCurrentDocumentContent(documentId)
		}
	})
}
