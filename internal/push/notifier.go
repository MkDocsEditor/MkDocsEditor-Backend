package push

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/activity"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
)

const (
	// DefaultDelay is how long after the last change of a feed entry its push is sent
	DefaultDelay = time.Minute
	// sentKeep is how long the notifier remembers which accounts got the push of an entry.
	// Editing sessions end after 15 minutes without changes.
	sentKeep = 24 * time.Hour
	// MsgFeedChanged is the type of the push payload
	MsgFeedChanged = "feed-changed"
	topicFeed      = "feed"
)

// Payload is the decrypted content of a push
type Payload struct {
	Type    string `json:"type"`
	EntryID string `json:"entryId"`
}

// Notifier pushes feed entries to the accounts that follow the document, see docs/push.md
type Notifier struct {
	sender   *Sender
	activity *activity.Store
	auth     *auth.Service
	// Delay is the time after the last change of an entry before it is pushed
	Delay time.Duration

	mu      sync.Mutex
	stopped bool
	timers  map[int64]*time.Timer
	// sent holds, per entry, when the push went out to which accounts
	sent map[int64]sentEntry
	jobs sync.WaitGroup
	// sendMu lets one entry be delivered at a time
	sendMu sync.Mutex
}

type sentEntry struct {
	at       time.Time
	accounts map[string]bool
}

// NewNotifier creates a notifier and subscribes it to the changes of the feed
func NewNotifier(sender *Sender, store *activity.Store, authenticator *auth.Service) *Notifier {
	n := &Notifier{sender: sender, activity: store, auth: authenticator, Delay: DefaultDelay,
		timers: map[int64]*time.Timer{}, sent: map[int64]sentEntry{}}
	store.AddEntryListener(n.entryChanged)
	return n
}

// Sender returns the sender of the notifier
func (n *Notifier) Sender() *Sender {
	return n.sender
}

// entryChanged (re)starts the timer of an entry
func (n *Notifier) entryChanged(id int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return
	}
	if t := n.timers[id]; t != nil {
		t.Stop()
	}
	n.timers[id] = time.AfterFunc(n.Delay, func() { n.fire(id) })
}

func (n *Notifier) fire(id int64) {
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return
	}
	delete(n.timers, id)
	n.jobs.Add(1)
	n.mu.Unlock()
	defer n.jobs.Done()
	n.sendMu.Lock()
	defer n.sendMu.Unlock()
	if err := n.deliver(id); err != nil {
		log.Printf("Unable to push feed entry %d: %v", id, err)
	}
}

// Close stops the pending pushes and waits for the running ones
func (n *Notifier) Close() {
	n.mu.Lock()
	n.stopped = true
	for _, t := range n.timers {
		t.Stop()
	}
	n.timers = map[int64]*time.Timer{}
	n.mu.Unlock()
	n.jobs.Wait()
}

// deliver pushes an entry to the accounts that follow its document and did not get it yet
func (n *Notifier) deliver(id int64) error {
	entry, err := n.activity.Entry(id)
	if err != nil || entry == nil || entry.ItemType != "document" || entry.Kind == activity.KindCreated {
		return err
	}
	family, watchers, err := n.activity.Watchers(entry.Path, entry.OldPath)
	if err != nil {
		return err
	}
	subs, err := n.activity.Subscriptions("")
	if err != nil {
		return err
	}
	payload, err := json.Marshal(Payload{Type: MsgFeedChanged, EntryID: strconv.FormatInt(id, 10)})
	if err != nil {
		return err
	}

	now := time.Now()
	n.mu.Lock()
	for k, v := range n.sent {
		if now.Sub(v.at) > sentKeep {
			delete(n.sent, k)
		}
	}
	sent := n.sent[id]
	if sent.accounts == nil {
		sent = sentEntry{at: now, accounts: map[string]bool{}}
		n.sent[id] = sent
	}
	done := map[string]bool{}
	for a := range sent.accounts {
		done[a] = true
	}
	n.mu.Unlock()

	// every device of an account gets the push, so the accounts are marked afterwards
	pushed := map[string]bool{}
	defer func() {
		n.mu.Lock()
		for a := range pushed {
			sent.accounts[a] = true
		}
		n.mu.Unlock()
	}()
	for _, sub := range subs {
		account := sub.AccountID
		if done[account] || !n.recipient(entry, account, family, watchers) {
			continue
		}
		if !n.sessionValid(sub) {
			log.Printf("Removing the push subscription of an ended session of %s", account)
			if err := n.activity.DeleteSubscription(sub.Endpoint); err != nil {
				log.Printf("Unable to remove a push subscription: %v", err)
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
		result, err := n.sender.Send(ctx, sub.Endpoint, sub.P256DH, sub.Auth, Message{Payload: payload, Topic: topicFeed})
		cancel()
		switch result {
		case Delivered:
			if err := n.activity.SubscriptionPushed(sub.Endpoint); err != nil {
				log.Printf("Unable to save a push: %v", err)
			}
		case Gone:
			log.Printf("Removing a push subscription of %s: the push service no longer knows it", account)
			if err := n.activity.DeleteSubscription(sub.Endpoint); err != nil {
				log.Printf("Unable to remove a push subscription: %v", err)
			}
		default:
			log.Printf("Push to a subscription of %s failed: %v", account, err)
		}
		pushed[account] = true
	}
	return nil
}

// recipient tells whether an account gets the push of an entry: another account changed it,
// and the account follows the document. The server's pseudo users alone are no reason.
func (n *Notifier) recipient(entry *activity.Entry, account string, family bool, watchers []string) bool {
	other := false
	for _, a := range entry.Authors {
		if a != account && !strings.HasPrefix(a, ":") {
			other = true
		}
	}
	if !other || !family && !slices.Contains(watchers, account) {
		return false
	}
	u := n.auth.UserByID(account)
	return u != nil && u.Status == auth.StatusActive
}

// sessionValid tells whether the session of a subscription still exists
func (n *Notifier) sessionValid(sub activity.Subscription) bool {
	sess, err := n.auth.SessionByID(sub.SessionID)
	if errors.Is(err, auth.ErrNotFound) {
		return false
	}
	if err != nil {
		// unknown, try again next time
		return true
	}
	now := time.Now()
	return sess.AccountID == sub.AccountID && now.Before(sess.ExpiresAt) && (sess.IdleExpiresAt.IsZero() || now.Before(sess.IdleExpiresAt))
}
