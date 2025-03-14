package backend

import (
	"encoding/base64"
	"errors"
	"fmt"
	automerge "github.com/automerge/automerge-go"
	"github.com/gorilla/websocket"
	"log"
	mutexSync "sync"
)

type (
	SyncRequest struct {
		Type       string `json:"type" xml:"type" form:"type" query:"type"`
		RequestId  string `json:"requestId" xml:"requestId" form:"requestId" query:"requestId"`
		DocumentId string `json:"documentId" xml:"documentId" form:"documentId" query:"documentId"`
		// *automerge.Doc as base64 encoded string
		DocumentState string `json:"documentState" xml:"documentState" form:"documentState" query:"documentState"`
		// *automerge.SyncMessage as base64 encoded string
		SyncMessage string `json:"syncMessage" xml:"syncMessage" form:"syncMessage" query:"syncMessage"`
	}
)

const (
	ContentPath = "content"
)

func (s SyncRequest) GetSyncMessageBytes() ([]byte, error) {
	decodedBytes, err := base64.StdEncoding.DecodeString(s.SyncMessage)
	if err != nil {
		return nil, err
	}
	return decodedBytes, nil
}

// AutomergeSyncManager manages processing of SyncRequests from clients
type AutomergeSyncManager struct {
	treeManager                *TreeManager
	websocketConnectionManager *WebsocketConnectionManager

	// automergeDocuments client connection -> automerge.Doc
	automergeDocuments map[*websocket.Conn]*automerge.Doc

	// lock for synchronizing the tree to the disk
	lock mutexSync.RWMutex
}

func NewAutomergeSyncManager(
	treeManager *TreeManager,
) *AutomergeSyncManager {
	s := &AutomergeSyncManager{
		treeManager:        treeManager,
		automergeDocuments: make(map[*websocket.Conn]*automerge.Doc),
	}

	return s
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
func (sm *AutomergeSyncManager) initClient(conn *websocket.Conn, document *automerge.Doc) {
	sm.automergeDocuments[conn] = document
}

// removes the shadow for the given client
func (sm *AutomergeSyncManager) removeClient(conn *websocket.Conn) {
	delete(sm.automergeDocuments, conn)
}

func (sm *AutomergeSyncManager) getDocument(documentId string) (doc *automerge.Doc, err error) {
	//doc, err := automerge.Load()
	doc = automerge.New()
	// doc.NewText(documentId)

	d := sm.treeManager.GetDocument(documentId)

	text := doc.Path(ContentPath).Text()
	err = text.Set(d.Content)
	if err != nil {
		return nil, err
	}

	return doc, err
}

// handles incoming edit requests from the client
func (sm *AutomergeSyncManager) handleSyncRequest(client *websocket.Conn, syncRequest SyncRequest) (err error) {
	documentId := syncRequest.DocumentId

	// get/create automerge document based on the document id
	automergeDocument, err := sm.getDocument(documentId)
	if err != nil {
		log.Printf("%v: error getting document: %v", client.RemoteAddr(), err)
		return err
	}
	text := automergeDocument.Path(ContentPath).Text()
	syncState := automerge.NewSyncState(automergeDocument)
	// sm.automergeDocuments[client]

	syncMessageBytes, err := syncRequest.GetSyncMessageBytes()
	if err != nil {
		log.Printf("%v: error getting sync message bytes: %v", client.RemoteAddr(), err)
		return err
	}
	_, err = syncState.ReceiveMessage(syncMessageBytes)
	if err != nil {
		log.Printf("%v: error receiving sync state: %v", client.RemoteAddr(), err)
		return err
	}

	// NOTE: this is not needed when using the SyncState API
	// apply patches to the automerge document
	//for _, change := range syncRequest.SyncMessage.Changes() {
	//	err := automergeDocument.Apply(change)
	//	if err != nil {
	//		return err
	//	}
	//}

	// then patch the server document version
	d := sm.treeManager.GetDocument(documentId)
	patchedText := text.String()
	if d.Content != patchedText {
		// TODO: maybe we need to save the automerge documents here too?
		defer sm.saveCurrentDocumentContent(documentId)
	}
	d.Content = patchedText

	//err = sm.sendSyncRequestResponse(client, documentId)
	//if err != nil {
	//	log.Printf("%v: error sending response: %v", client.RemoteAddr(), err)
	//	return err
	//}

	return err
}

// send the latest document state to the client
func (sm *AutomergeSyncManager) sendInitialTextResponse(client *websocket.Conn, document *Document) (err error) {
	automergeDocument, err := sm.getDocument(document.ID)

	heads := automergeDocument.Heads()

	syncState := automerge.NewSyncState(automergeDocument)

	documentContentText := automergeDocument.Path(ContentPath).Text()
	err = documentContentText.Set(document.Content)
	if err != nil {
		return err
	}

	commitMessage := "Initial Text"
	commit, err := automergeDocument.Commit(commitMessage)
	log.Printf("Commit: %v", commit)
	if err != nil {
		return err
	}

	changes, err := automergeDocument.Changes(heads...)
	changes[0].Save()

	// set initial state in backend
	sm.initClient(client, automergeDocument)

	syncStateMessage, valid := syncState.GenerateMessage()
	if valid == false {
		log.Printf("Error generating sync state message: %v", err)
		return err
	}

	// Write current document state to the client
	request := SyncRequest{
		Type:          TypeInitialContent,
		DocumentId:    document.ID,
		RequestId:     "",
		DocumentState: encodeBase64(automergeDocument.Save()),
		SyncMessage:   encodeBase64(syncStateMessage.Bytes()),
	}
	err = client.WriteJSON(request)
	if err != nil {
		log.Printf("%v: error writing initial content response: %v", client.RemoteAddr(), err)
		return err
	}

	return
}

func encodeBase64(buffer []byte) string {
	return base64.StdEncoding.EncodeToString(buffer)
}

// responds to a client with the changes from the server site document version
func (sm *AutomergeSyncManager) sendSyncRequestResponse(client *websocket.Conn, documentId string) (err error) {
	// d := sm.treeManager.GetDocument(documentId)

	automergeDocument, err := sm.getDocument(documentId)
	if err != nil {
		log.Printf("%v: error getting document: %v", client.RemoteAddr(), err)
		return err
	}

	syncState := automerge.NewSyncState(automergeDocument)
	syncStateMessage, valid := syncState.GenerateMessage()
	if valid == false {
		log.Printf("Error generating sync state message: %v", err)
		return err
	}

	return sm.websocketConnectionManager.syncStateToClient(
		client,
		SyncRequest{
			Type:          TypeSyncRequest,
			RequestId:     "",
			DocumentId:    documentId,
			DocumentState: encodeBase64(automergeDocument.Save()),
			SyncMessage:   encodeBase64(syncStateMessage.Bytes()),
		})
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
	sm.websocketConnectionManager.SetOnSyncRequestMessageListener(func(client *websocket.Conn, request SyncRequest) error {
		fmt.Println("Incoming sync message from client", client)
		return sm.handleSyncRequest(client, request)
	})
	sm.websocketConnectionManager.SetOnClientDisconnectedListener(func(client *websocket.Conn, documentId string, remainingConnections uint) {
		fmt.Println("Client disconnected", client)
		sm.removeClient(client)
		if remainingConnections <= 0 {
			sm.saveCurrentDocumentContent(documentId)
		}
	})
}
