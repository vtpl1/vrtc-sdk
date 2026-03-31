package relayhub_test

import (
	"context"
	"testing"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/relayhub"
	"github.com/vtpl1/vrtc-sdk/lifecycle"
)

func TestInterfaceImplementations(t *testing.T) {
	var (
		_ av.RelayHub            = (*relayhub.RelayHub)(nil)
		_ lifecycle.StartStopper = (*relayhub.RelayHub)(nil)
		_ av.DemuxCloser         = (*relayhub.Relay)(nil)
		_ av.MuxCloser           = (*relayhub.Consumer)(nil)
		_ av.CodecChanger        = (*relayhub.Consumer)(nil)
	)
}

func TestNew(t *testing.T) {
	ctx := t.Context()

	sm := relayhub.New(func(_ context.Context, _ string) (av.DemuxCloser, error) {
		return nil, nil
	}, nil)
	sm.Start(ctx)

	if sm == nil {
		t.Fatal("expected non-nil RelayHub")
	}

	if sm.GetActiveRelayCount(ctx) != 0 {
		t.Fatalf("expected 0 active producers, got %d", sm.GetActiveRelayCount(ctx))
	}
}
