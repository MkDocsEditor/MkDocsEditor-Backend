package backend

import (
	"crypto/md5"
	"errors"
	"fmt"
	automerge "github.com/automerge/automerge-go"
	"github.com/gorilla/websocket"
	"golang.org/x/text/encoding/unicode"
	"log"
	"strings"
	mutexSync "sync"
)

type (
	SyncStateRequest struct {
		Type       string               `json:"type" xml:"type" form:"type" query:"type"`
		RequestId  string               `json:"requestId" xml:"requestId" form:"requestId" query:"requestId"`
		DocumentId string               `json:"documentId" xml:"documentId" form:"documentId" query:"documentId"`
		SyncState  *automerge.SyncState `json:"syncState" xml:"syncState" form:"syncState" query:"syncState"`
	}
)

// AutomergeSyncManager manages processing of EditRequests from clients
type AutomergeSyncManager struct {
	treeManager                *TreeManager
	websocketConnectionManager *WebsocketConnectionManager

	// ServerShadows client connection -> server shadow
	ServerShadows map[*websocket.Conn]string

	// lock for synchronizing the tree to the disk
	lock mutexSync.RWMutex
}

func NewAutomergeSyncManager(
	treeManager *TreeManager,
) *AutomergeSyncManager {
	AutomergeSyncManager := &AutomergeSyncManager{
		treeManager:   treeManager,
		ServerShadows: make(map[*websocket.Conn]string),
	}

	return AutomergeSyncManager
}

func (sm *AutomergeSyncManager) IsItemBeingEditedRecursive(s *Section) (err error) {
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
func (sm *AutomergeSyncManager) initClient(conn *websocket.Conn, shadowContent string) {
	sm.ServerShadows[conn] = shadowContent
}

// removes the shadow for the given client
func (sm *AutomergeSyncManager) removeClient(conn *websocket.Conn) {
	delete(sm.ServerShadows, conn)
}

func (sm *AutomergeSyncManager) getDocument(documentId string) (err error) {
	//doc, err := automerge.Load()
	doc := automerge.New()
	doc.
		//NewText(documentId)

		text := doc.Path("collection").Text()
}

// handles incoming edit requests from the client
func (sm *AutomergeSyncManager) handleEditRequest(client *websocket.Conn, editRequest SyncStateRequest) (err error) {
	documentId := editRequest.DocumentId

	// get/create automerge document based on the document id
	automergeDocument := getDocument(documentId)
	// sm.ServerShadows[client]

	// apply patches to the automerge document
	automergeDocument.ApplyChanges(editRequest.Patches)

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

// send the latest document state to the client
func (sm *AutomergeSyncManager) sendInitialTextResponse(client *websocket.Conn, document *Document) (err error) {
	// set initial state in backend
	sm.initClient(client, document.Content)

	automergeDocument := automerge.New()

	automergeText := automerge.NewText(document.Content)

	documentContentText, err := automerge.As[*automerge.Text](automergeDocument.Root())
	if err != nil {
		return err
	}
	err = documentContentText.Set(document.Content)
	if err != nil {
		return err
	}

	commitMessage := "Initial commit"
	commit, err := automergeDocument.Commit(commitMessage)
	if err != nil {
		return err
	}
	serializedDocument := automergeDocument.Save()

	syncState := automerge.NewSyncState(automergeDocument)
	shadowChecksum := sm.calculateChecksum(document.Content)

	// Write current document state to the client
	err = client.WriteJSON(SyncStateRequest{
		Type:           TypeInitialContent,
		DocumentId:     document.ID,
		RequestId:      "",
		SyncState:      syncState,
		ShadowChecksum: shadowChecksum,
	})
	if err != nil {
		log.Printf("%v: error writing initial content response: %v", client.RemoteAddr(), err)
		return err
	}

	return
}

// responds to a client with the changes from the server site document version
func (sm *AutomergeSyncManager) sendEditRequestResponse(client *websocket.Conn, documentId string) (err error) {
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
func (sm *AutomergeSyncManager) calculateChecksum(text string) string {
	encoder := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewEncoder()
	utf16, err := encoder.String(text)
	if err != nil {
		log.Printf("Error encoding String to UTF-16: %v", err)
	}

	hash := md5.Sum([]byte(utf16))
	checksum := fmt.Sprintf("%02x", hash[:])
	return strings.ToLower(checksum)
}

func (sm *AutomergeSyncManager) saveCurrentDocumentContent(documentId string) {
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

func (sm *AutomergeSyncManager) SetWebsocketConnectionManager(manager *WebsocketConnectionManager) {
	sm.websocketConnectionManager = manager

	sm.websocketConnectionManager.SetOnNewClientListener(func(client *websocket.Conn, document *Document) error {
		fmt.Println("New client connected", client)
		return sm.sendInitialTextResponse(client, document)
	})
	sm.websocketConnectionManager.SetOnIncomingMessageListener(func(client *websocket.Conn, request EditRequest) error {
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
