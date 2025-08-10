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

// Listen starts a background loop that connects (and reconnects) to Twitch IRC,
// and pushes incoming PRIVMSG messages onto the returned channel.
// Close everything by canceling ctx. Both channels will be closed on exit.
func Listen(ctx context.Context, cfg Config) (<-chan ChatMessage, <-chan error, error) {
	if cfg.Username == "" || cfg.Token == "" || cfg.Channel == "" {
		return nil, nil, fmt.Errorf("missing cfg fields: Username: %s, Token: %s, Channel: %s are required", cfg.Username, cfg.Token, cfg.Channel)
	}

	msgCh := make(chan ChatMessage, 256) // bump if you expect bursts
	errCh := make(chan error, 1)

	go func() {
		defer close(msgCh)
		defer close(errCh)

		backoff := time.Second
		for {
			if err := runOnce(ctx, cfg, msgCh); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				select {
				case errCh <- err:
				default:
				}
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()

	return msgCh, errCh, nil
}

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

	// auth + caps + join
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

	go func() {
		for {
			line, e := reader.ReadString('\n')
			if e != nil {
				errCh <- e
				return
			}
			line = strings.TrimRight(line, "\r\n")

			// PING -> PONG
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
				var ch string
				if len(msg.Params) > 0 {
					ch = msg.Params[0]
				}
				display := msg.Tags["display-name"]
				if display == "" {
					display = nickFromPrefix(msg.Prefix)
				}
				select {
				case out <- ChatMessage{
					Channel: ch,
					User:    display,
					Text:    msg.Trailing,
					Tags:    msg.Tags,
					Raw:     msg.Raw,
				}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

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

// --- minimal IRC parsing helpers ---

type ircMessage struct {
	Raw      string
	Tags     map[string]string
	Prefix   string
	Cmd      string
	Params   []string
	Trailing string
}

func parseIRC(raw string) ircMessage {
	msg := ircMessage{Raw: raw, Tags: map[string]string{}}
	rest := raw

	// tags
	if strings.HasPrefix(rest, "@") {
		if sp := strings.Index(rest, " "); sp != -1 {
			tagPart := rest[1:sp]
			rest = strings.TrimSpace(rest[sp+1:])
			for kv := range strings.SplitSeq(tagPart, ";") {
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

	// prefix
	if strings.HasPrefix(rest, ":") {
		if sp := strings.Index(rest, " "); sp != -1 {
			msg.Prefix = rest[1:sp]
			rest = strings.TrimSpace(rest[sp+1:])
		} else {
			return msg
		}
	}

	// command
	if sp := strings.Index(rest, " "); sp != -1 {
		msg.Cmd = rest[:sp]
		rest = strings.TrimSpace(rest[sp+1:])
	} else {
		msg.Cmd = rest
		return msg
	}

	// params + trailing
	if i := strings.Index(rest, " :"); i != -1 {
		params := strings.TrimSpace(rest[:i])
		if params != "" {
			msg.Params = strings.Split(params, " ")
		}
		msg.Trailing = rest[i+2:]
	} else {
		if rest != "" {
			msg.Params = strings.Split(rest, " ")
		}
	}
	return msg
}

func nickFromPrefix(prefix string) string {
	if i := strings.Index(prefix, "!"); i > 0 {
		return prefix[:i]
	}
	return prefix
}
