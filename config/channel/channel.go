// Package channel provides types and interfaces for resolving camera/stream
// channel details from pluggable sources (JSON file, database, etc.).
package channel

import (
	"context"
	"errors"
)

// ErrChannelNotFound is returned by ChannelProvider.GetChannel when the
// requested ID does not exist in the source.
var ErrChannelNotFound = errors.New("channel: not found")

// Channel describes one camera or stream source.
// ID is the sourceID used throughout the stream manager; it must be unique.
type Channel struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	StreamURL string            `json:"streamUrl"`
	Username  string            `json:"username,omitempty"`
	Password  string            `json:"password,omitempty"`
	SiteID    int               `json:"siteId"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// ChannelProvider is the single interface all channel sources must satisfy.
// Implementations are expected to be safe for concurrent use.
type ChannelProvider interface {
	// GetChannel returns the Channel for the given id, or ErrChannelNotFound.
	GetChannel(ctx context.Context, id string) (Channel, error)

	// ListChannels returns all channels known to this provider.
	ListChannels(ctx context.Context) ([]Channel, error)

	// Close releases any held resources (DB connections, file handles, etc.).
	Close() error
}

// ChannelWriter extends ChannelProvider with create/update/delete operations.
// Not all providers need to implement this — file, MySQL, and MongoDB do.
type ChannelWriter interface {
	ChannelProvider

	// SaveChannel creates or updates a channel. If a channel with the same ID
	// already exists it is overwritten.
	SaveChannel(ctx context.Context, ch Channel) error

	// DeleteChannel removes the channel with the given id.
	// Returns ErrChannelNotFound if the id does not exist.
	DeleteChannel(ctx context.Context, id string) error
}
