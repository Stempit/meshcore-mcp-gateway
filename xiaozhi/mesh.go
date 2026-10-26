package xiaozhi

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stempit/meshcore-companion/companion"
	"github.com/stempit/meshcore-go"
)

// ErrUnknownRecipient reports that a dm/channel destination did not resolve
// to a known contact or channel.
var ErrUnknownRecipient = errors.New("recipient not found")

// Mesh is the radio side of the voice tools. It exists so the MCP layer is
// independent of how the mesh is reached: DirectMesh serves it from a local
// meshcore client + inbox (no HTTP gateway involved); tests provide a fake.
type Mesh interface {
	// MessagesSince returns pending messages newer than after, plus the
	// newest assigned ID (a safe next cursor).
	MessagesSince(after int64) ([]companion.Message, int64)
	// SendDM / SendChannel resolve to (contact name, hex key prefix, or
	// channel name/index) and transmit, blocking until the device confirms
	// or the retries give up.
	SendDM(to, text string, ack bool) error
	SendChannel(to, text string, ack bool) error
	// Contacts / Channels are the directory listings.
	Contacts() []string
	Channels() []ChannelRef
	// SearchContacts returns the names of all contacts matching the query
	// fragment (case/emoji/punctuation ignored, hex key prefix works).
	SearchContacts(query string) []string
}

// ChannelRef names a configured channel in listings.
type ChannelRef struct {
	Index int
	Name  string
}

// DirectMesh implements Mesh against a local meshcore client and the shared
// inbox: the bridge opens its own device connection, independent of any other
// gateway.
type DirectMesh struct {
	client *meshcore.MeshCoreClient
	inbox  *companion.Inbox
	opts   DirectMeshOptions
}

// DirectMeshOptions carries the host-configured send pacing for DirectMesh.
type DirectMeshOptions struct {
	// ChannelTransmissions is the MAXIMUM number of times each channel send
	// is transmitted (deduplicated by receivers) to improve delivery against
	// silent RF drops. 0 defaults to 3. It mirrors the Connection config
	// section of the host application.
	ChannelTransmissions int
}

// NewDirectMesh builds the local radio backend for the voice tools.
func NewDirectMesh(client *meshcore.MeshCoreClient, inbox *companion.Inbox, opts DirectMeshOptions) *DirectMesh {
	return &DirectMesh{client: client, inbox: inbox, opts: opts}
}

// Send pacing mirrors the bot dispatcher's parameters (internal/dispatch):
// the retry counts and per-attempt confirmation timeouts the radio link is
// known to tolerate.
const (
	sendTimeoutPerTry = 3 * time.Second
	savedPathTries    = 3
	resetPathTries    = 3
)

func (m *DirectMesh) MessagesSince(after int64) ([]companion.Message, int64) {
	return m.inbox.Since(after), m.inbox.LastID()
}

func (m *DirectMesh) SendDM(to, text string, ack bool) error {
	contact, err := m.findContact(to)
	if err != nil {
		return err // *meshcore.AmbiguousMatchError — the tool voices the candidates
	}
	if contact == nil {
		return fmt.Errorf("%w: %s", ErrUnknownRecipient, to)
	}
	return m.client.SendMessageWithRetry(contact, text, ack, savedPathTries, resetPathTries, sendTimeoutPerTry)
}

func (m *DirectMesh) SendChannel(to, text string, ack bool) error {
	ch, err := m.findChannel(to)
	if err != nil {
		return err
	}
	if ch == nil {
		return fmt.Errorf("%w: %s", ErrUnknownRecipient, to)
	}
	transmissions := m.opts.ChannelTransmissions
	if transmissions <= 0 {
		transmissions = 3
	}
	return m.client.SendChannelMessageWithRetry(ch, text, ack, 1, transmissions, sendTimeoutPerTry)
}

// findContact resolves a destination by exact contact name, by hex public-key
// prefix, or — falling back — by a loose name match that forgives case, emoji
// and punctuation ("Yorick" finds "Yorick 💀").
func (m *DirectMesh) findContact(to string) (*meshcore.Contact, error) {
	if c := m.client.FindContactByName(to); c != nil {
		return c, nil
	}
	if key, err := hex.DecodeString(strings.ToLower(to)); err == nil && len(key) > 0 {
		if c := m.client.FindContactByKeyPrefix(key); c != nil {
			return c, nil
		}
	}
	return m.client.FindContactByNameLoose(to)
}

// findChannel resolves a destination by exact channel name, numeric index, or
// loose name match (see findContact).
func (m *DirectMesh) findChannel(to string) (*meshcore.Channel, error) {
	if ch := m.client.GetChannelByName(to); ch != nil {
		return ch, nil
	}
	if idx, err := strconv.Atoi(to); err == nil {
		if ch, ok := m.client.GetChannels()[idx]; ok && !ch.IsEmpty {
			return ch, nil
		}
	}
	return m.client.FindChannelByNameLoose(to)
}

func (m *DirectMesh) Contacts() []string {
	contacts := m.client.GetContacts()
	names := make([]string, 0, len(contacts))
	for _, c := range contacts {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}

func (m *DirectMesh) SearchContacts(query string) []string {
	hits := m.client.SearchContacts(query)
	names := make([]string, 0, len(hits))
	for _, c := range hits {
		names = append(names, c.Name)
	}
	return names
}

func (m *DirectMesh) Channels() []ChannelRef {
	channels := m.client.GetChannels()
	refs := make([]ChannelRef, 0, len(channels))
	for _, ch := range channels {
		if ch.IsEmpty {
			continue
		}
		refs = append(refs, ChannelRef{Index: ch.Index, Name: ch.Name})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Index < refs[j].Index })
	return refs
}
