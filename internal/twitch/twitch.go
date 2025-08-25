package twitch

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const twitchHost = "irc.chat.twitch.tv:6697"

type Config struct {
	Username string // lowercase
	Token    string // includes "oauth:..."
	Channel  string // without '#'
}

type ChatMessage struct {
	Channel string
	User    string
	Text    string
	Tags    map[string]string
	Raw     string
}

// Listen connects to Twitch IRC and streams chat messages.
// It returns two channels:
//   - ChatMessage channel with incoming messages
//   - error channel for connection or parsing errors
// Callers should stop listening by canceling the provided context.
func Listen(ctx context.Context, cfg Config) (<-chan ChatMessage, <-chan error, error) {
	// Ensure config has the required fields
	if cfg.Username == "" || cfg.Token == "" || cfg.Channel == "" {
		return nil, nil, fmt.Errorf(
			"missing cfg fields: Username=%q, Token=%q, Channel=%q must all be set",
			cfg.Username, cfg.Token, cfg.Channel,
		)
	}

	msgCh := make(chan ChatMessage, 256) // buffered for bursts
	errCh := make(chan error, 1)

	go func() {
		defer close(msgCh)
		defer close(errCh)

		backoff := time.Second

		for {
			// One connection attempt
			if err := runOnce(ctx, cfg, msgCh); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				// Non-blocking send to errCh
				select {
				case errCh <- err:
				default:
				}
			}

			// Exit on context cancel, otherwise backoff before retry
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}

			// Exponential backoff up to 30s
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()

	return msgCh, errCh, nil
}

// runOnce performs one Twitch IRC connection and runs until disconnected.
func runOnce(ctx context.Context, cfg Config, out chan<- ChatMessage) error {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", twitchHost, &tls.Config{})
	if err != nil {
		return err
	}
	defer conn.Close()

	write := func(s string) error {
		_, err := conn.Write([]byte(s + "\r\n"))
		return err
	}

	// Authenticate and join channel
	if err := write("PASS " + cfg.Token); err != nil {
		return err
	}
	if err := write("NICK " + cfg.Username); err != nil {
		return err
	}
	if err := write("CAP REQ :twitch.tv/tags twitch.tv/commands"); err != nil {
		return err
	}
	if err := write("JOIN #" + strings.ToLower(cfg.Channel)); err != nil {
		return err
	}

	reader := bufio.NewReader(conn)
	errCh := make(chan error, 1)

	pingTicker := time.NewTicker(4 * time.Minute)
	defer pingTicker.Stop()

	// Reader goroutine
	go func() {
		for {
			line, e := reader.ReadString('\n')
			if e != nil {
				errCh <- e
				return
			}
			line = strings.TrimRight(line, "\r\n")

			// Respond to server pings
			if strings.HasPrefix(line, "PING ") {
				_ = write(strings.Replace(line, "PING", "PONG", 1))
				continue
			}

			msg := parseIRC(line)
			switch msg.Cmd {
			case "RECONNECT":
				errCh <- fmt.Errorf("server requested RECONNECT")
				return

			case "PRIVMSG":
				ch := ""
				if len(msg.Params) > 0 {
					ch = msg.Params[0]
				}
				user := msg.Tags["display-name"]
				if user == "" {
					user = nickFromPrefix(msg.Prefix)
				}

				chatMsg := ChatMessage{
					Channel: ch,
					User:    user,
					Text:    msg.Trailing,
					Tags:    msg.Tags,
					Raw:     msg.Raw,
				}

				select {
				case out <- chatMsg:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// Main loop: handle ctx, ping keepalive, or reader errors
	for {
		select {
		case <-ctx.Done():
			return context.Canceled
		case <-pingTicker.C:
			if err := write("PING :keepalive"); err != nil {
				return err
			}
		case e := <-errCh:
			return e
		}
	}
}

// --- IRC parsing helpers ---

type ircMessage struct {
	Raw      string
	Tags     map[string]string
	Prefix   string
	Cmd      string
	Params   []string
	Trailing string
}

// parseIRC decodes a raw IRC line into structured fields.
func parseIRC(raw string) ircMessage {
	msg := ircMessage{Raw: raw, Tags: map[string]string{}}
	rest := raw

	// Parse tags (e.g., @badge-info=…)
	if strings.HasPrefix(rest, "@") {
		if sp := strings.Index(rest, " "); sp != -1 {
			tagPart := rest[1:sp]
			rest = strings.TrimSpace(rest[sp+1:])
			for _, kv := range strings.Split(tagPart, ";") {
				if kv == "" {
					continue
				}
				if eq := strings.Index(kv, "="); eq != -1 {
					msg.Tags[kv[:eq]] = kv[eq+1:]
				} else {
					msg.Tags[kv] = ""
				}
			}
		}
	}

	// Parse prefix (e.g., :nickname!)
	if strings.HasPrefix(rest, ":") {
		if sp := strings.Index(rest, " "); sp != -1 {
			msg.Prefix = rest[1:sp]
			rest = strings.TrimSpace(rest[sp+1:])
		} else {
			return msg
		}
	}

	// Parse command
	if sp := strings.Index(rest, " "); sp != -1 {
		msg.Cmd = rest[:sp]
		rest = strings.TrimSpace(rest[sp+1:])
	} else {
		msg.Cmd = rest
		return msg
	}

	// Params + trailing message
	if i := strings.Index(rest, " :"); i != -1 {
		if params := strings.TrimSpace(rest[:i]); params != "" {
			msg.Params = strings.Split(params, " ")
		}
		msg.Trailing = rest[i+2:]
	} else if rest != "" {
		msg.Params = strings.Split(rest, " ")
	}

	return msg
}

// nickFromPrefix extracts the nickname from an IRC prefix.
func nickFromPrefix(prefix string) string {
	if i := strings.Index(prefix, "!"); i > 0 {
		return prefix[:i]
	}
	return prefix
}

