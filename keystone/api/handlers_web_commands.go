package api

// Slash commands in the built-in web chats.
//
// The same commands, answered by the same code, as over a chat transport
// (handlers_chat_commands.go): deterministic, no model runs, nothing is
// proposed. What a person may type does not depend on where they typed
// it — only on which Agent they are talking to, because every command
// still declares the SURFACES that offer it and a household Agent does
// not answer home-lab commands anywhere (ADR-W026 A1).
//
// The two web chats reach this from different handlers:
//
//   - the household dashboard's assistant (assistantAsk), after its own
//     ceiling has resolved the Agent for the member;
//   - the Console (runNamedButlerSession), after Butlers.Resolve — the
//     same resolution, access check and surface ceiling Serve applies to
//     a question.
//
// Either way a command reaches exactly the Agents a question could.

import (
	"github.com/chickenrat17/wildstone-homelab/keystone/butlerlive"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// webConnection is the Connection a web chat answers on. Built-in web
// chats have no Connection row (their Seq is 0, as a trusted built-in
// destination's always is); what the commands read from it is the Agent,
// its surface and class, and the transport to name in /whoami.
func webConnection(b runtimedb.Butler, connection string) runtimedb.Connection {
	return runtimedb.Connection{
		Butler:     b.Name,
		Transport:  webTransportLabel(connection),
		Surface:    b.Surface,
		AgentClass: b.AgentClass,
	}
}

func webTransportLabel(connection string) string {
	switch connection {
	case portal.Connection:
		return "the household dashboard"
	case butlerlive.ConsoleConnection:
		return "the Console"
	}
	return connection
}

// runWebChatCommand answers text if it is a slash command, for member
// talking to b over the built-in web chat named connection. b must
// already have passed the caller's ceiling.
func (s *Server) runWebChatCommand(b runtimedb.Butler, member, connection, text string) (askResponse, bool) {
	if b.Name == "" {
		return askResponse{}, false
	}
	effect := &webCommandEffect{}
	body, _, handled := s.runChatCommand(chatCommandCtx{
		At:       webConnection(b, connection),
		Identity: member,
		Web:      effect,
	}, text)
	if !handled {
		return askResponse{}, false
	}
	name, _, _ := parseChatCommand(text)
	return askResponse{
		FinalText:       body,
		SessionID:       effect.ResumeSession,
		Command:         name,
		NewConversation: effect.NewConversation,
	}, true
}

// runNamedWebCommand is runWebChatCommand for a Console turn, resolving
// the Agent exactly as Serve would. A Butlers without Resolve (a test
// double) answers no commands; an Agent that cannot be resolved — no
// access, the wrong surface, retired, or its model unreachable — is left
// to the ordinary path, which refuses it in the ordinary way.
func (s *Server) runNamedWebCommand(turn butlerlive.Turn) (askResponse, bool) {
	if _, ok := parseChatCommandName(turn.Text); !ok {
		return askResponse{}, false
	}
	resolver, ok := s.Butlers.(interface {
		Resolve(butlerlive.Turn) (runtimedb.Butler, error)
	})
	if !ok {
		return askResponse{}, false
	}
	b, err := resolver.Resolve(turn)
	if err != nil {
		return askResponse{}, false
	}
	return s.runWebChatCommand(b, turn.Member, turn.Connection.Name, turn.Text)
}

// parseChatCommandName reports whether text is a slash command at all,
// before anything is resolved for it.
func parseChatCommandName(text string) (string, bool) {
	name, _, ok := parseChatCommand(text)
	return name, ok
}
