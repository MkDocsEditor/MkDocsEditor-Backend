package backend

import (
	"errors"
	"fmt"
	automerge "github.com/automerge/automerge-go"
	"github.com/gorilla/websocket"
	"log"
	mutexSync "sync"
)

type (
	SyncRequest struct {
		Type       string                 `json:"type" xml:"type" form:"type" query:"type"`
		RequestId  string                 `json:"requestId" xml:"requestId" form:"requestId" query:"requestId"`
		DocumentId string                 `json:"documentId" xml:"documentId" form:"documentId" query:"documentId"`
		SyncState  *automerge.SyncMessage `json:"syncState" xml:"syncState" form:"syncState" query:"syncState"`
	}
)

// AutomergeSyncManager manages processing of SyncRequests from clients
type AutomergeSyncManager struct {
	treeManager                *TreeManager
	websocketConnectionManager *WebsocketConnectionManager

	// automergeDocuments client connection -> automerge.Doc
	automergeDocuments map[*websocket.Conn]string

	// lock for synchronizing the tree to the disk
	lock mutexSync.RWMutex
}

func NewAutomergeSyncManager(
	treeManager *TreeManager,
) *AutomergeSyncManager {
	s := &AutomergeSyncManager{
		treeManager:        treeManager,
		automergeDocuments: make(map[*websocket.Conn]string),
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
func (sm *AutomergeSyncManager) initClient(conn *websocket.Conn, shadowContent string) {
	sm.automergeDocuments[conn] = shadowContent
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

	text := doc.Path("content").Text()
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
	text := automergeDocument.Path("content").Text()
	syncState := automerge.NewSyncState(automergeDocument)
	// sm.automergeDocuments[client]

	_, err = syncState.ReceiveMessage(syncRequest.SyncState.Bytes())
	if err != nil {
		log.Printf("%v: error receiving sync state: %v", client.RemoteAddr(), err)
		return err
	}

	// apply patches to the automerge document
	for _, change := range syncRequest.SyncState.Changes() {
		err := automergeDocument.Apply(change)
		if err != nil {
			return err
		}
	}

	// then patch the server document version
	d := sm.treeManager.GetDocument(documentId)
	patchedText := text.String()
	if d.Content != patchedText {
		// TODO: maybe we need to save the automerge documents here too?
		defer sm.saveCurrentDocumentContent(documentId)
	}
	d.Content = patchedText

	err = sm.sendSyncRequestResponse(client, documentId)
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
	syncState := automerge.NewSyncState(automergeDocument)

	documentContentText, err := automerge.As[*automerge.Text](automergeDocument.RootMap().Get("content"))
	if err != nil {
		return err
	}
	err = documentContentText.Set(document.Content)
	if err != nil {
		return err
	}

	commitMessage := "Initial commit"
	commit, err := automergeDocument.Commit(commitMessage)
	log.Printf("Commit: %v", commit)
	if err != nil {
		return err
	}

	syncStateMessage, valid := syncState.GenerateMessage()
	if valid == false {
		log.Printf("Error generating sync state message: %v", err)
		return err
	}

	// Write current document state to the client
	err = client.WriteJSON(SyncRequest{
		Type:       TypeInitialContent,
		DocumentId: document.ID,
		RequestId:  "",
		SyncState:  syncStateMessage,
	})
	if err != nil {
		log.Printf("%v: error writing initial content response: %v", client.RemoteAddr(), err)
		return err
	}

	return
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
			Type:       TypeSyncRequest,
			RequestId:  "",
			DocumentId: documentId,
			SyncState:  syncStateMessage,
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
