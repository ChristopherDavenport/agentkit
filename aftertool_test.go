package agentkit_test

import (
	"context"
	"slices"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

// Two WithAfterToolCall hooks both run, in the order given, joined with
// agentturn.ChainAfterToolCall: the second sees the result as the first
// left it, and the chain's override is the second's. v0.0.19 kept the
// second hook alone. (#139)
func TestAfterToolCallHooksChainInOrder(t *testing.T) {
	var ran []string
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithAfterToolCall(func(_ context.Context, info agentturn.ToolResultInfo) (*agentturn.ToolOverride, error) {
			ran = append(ran, "first saw "+info.Result.Output.Text)
			return &agentturn.ToolOverride{Result: agenttool.Text("first")}, nil
		}),
		agentkit.WithAfterToolCall(func(_ context.Context, info agentturn.ToolResultInfo) (*agentturn.ToolOverride, error) {
			ran = append(ran, "second saw "+info.Result.Output.Text)
			return &agentturn.ToolOverride{Result: agenttool.Text(info.Result.Output.Text + ", then second")}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	hook := kit.Config().AfterToolCall
	if hook == nil {
		t.Fatal("no AfterToolCall")
	}
	o, err := hook(t.Context(), agentturn.ToolResultInfo{Result: agenttool.Text("tool")})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"first saw tool", "second saw first"}; !slices.Equal(ran, want) {
		t.Errorf("ran = %q, want %q", ran, want)
	}
	if o == nil || o.Result.Output.Text != "first, then second" {
		t.Errorf("override = %+v, want the second's over the first's", o)
	}
}

// A nil hook is passed over: it no longer clears the one before it.
// (#139 review F4)
func TestANilAfterToolCallKeepsTheEarlierHook(t *testing.T) {
	ran := false
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithAfterToolCall(func(context.Context, agentturn.ToolResultInfo) (*agentturn.ToolOverride, error) {
			ran = true
			return nil, nil
		}),
		agentkit.WithAfterToolCall(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	hook := kit.Config().AfterToolCall
	if hook == nil {
		t.Fatal("WithAfterToolCall(nil) cleared the earlier hook")
	}
	if _, err := hook(t.Context(), agentturn.ToolResultInfo{Result: agenttool.Text("tool")}); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("the earlier hook did not run")
	}
}
