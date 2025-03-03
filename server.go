package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	log "github.com/Sirupsen/logrus"

	"golang.org/x/crypto/ssh"
)

var sessions = struct {
	mu   sync.RWMutex
	keys map[string][]*publicKey
}{
	keys: make(map[string][]*publicKey),
}

func serve(config *ssh.ServerConfig, nConn net.Conn) {
	// Before use, a handshake must be performed on the incoming net.Conn
	conn, chans, reqs, err := ssh.NewServerConn(nConn, config)
	if err != nil {
		log.Warnln("Failed to handshake:", err)
		return
	}

	defer func() {
		sessions.mu.Lock()
		delete(sessions.keys, string(conn.SessionID()))
		sessions.mu.Unlock()
		conn.Close()
	}()

	// The incoming Request channel must be serviced
	go ssh.DiscardRequests(reqs)

	sessions.mu.RLock()
	keys := sessions.keys[string(conn.SessionID())]
	sessions.mu.RUnlock()

	// Service the incoming Channel channel
	for n := range chans {
		// Channels have a type, depending on the application level
		// protocol intended. In the case of a shell, the type is
		// "session" and ServerShell may be used to present a simple
		// terminal interface.
		if n.ChannelType() != "session" {
			n.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		channel, requests, err := n.Accept()
		if err != nil {
			log.Warnln("Could not accept channel:", err)
			continue
		}

		agentFwd, x11 := false, false
		reqLock := &sync.Mutex{}
		reqLock.Lock()
		timeout := time.AfterFunc(30*time.Second, func() { reqLock.Unlock() })

		go func(in <-chan *ssh.Request) {
			for req := range in {
				ok := false
				switch req.Type {
				case "shell":
					fallthrough
				case "pty-req":
					ok = true

					// "auth-agent-req@openssh.com" and "x11-req" always arrive
					// before the "pty-req", so we can go ahead now
					if timeout.Stop() {
						reqLock.Unlock()
					}

				case "auth-agent-req@openssh.com":
					agentFwd = true
				case "x11-req":
					x11 = true
				}

				if req.WantReply {
					req.Reply(ok, nil)
				}
			}
		}(requests)

		markBlacklistedKeys(keys)

		channel.Write([]byte(welcomeMsg))

		var table bytes.Buffer
		tabWriter := new(tabwriter.Writer)
		tabWriter.Init(&table, 5, 2, 2, ' ', 0)
		// Note that using tabwriter, columns are tab-terminated,
		// not tab-delimited
		fmt.Fprint(tabWriter, "Bits\tType\tFingerprint\tIssues\n")

		var issues string
		var blacklisted, weak, dsa bool
		for _, k := range keys {
			issues = "No known issues"
			length, err := k.BitLen()

			if err != nil {
				log.Errorf("Failed to determine key length for %s key: %s", k.key.Type(), err)
			}

			if k.key.Type() == ssh.KeyAlgoDSA {
				issues = "DSA KEY"
				dsa = true
			}

			if length < 2048 && k.key.Type() == ssh.KeyAlgoRSA {
				issues = "WEAK KEY LENGTH"
				weak = true
			}

			if k.blacklisted {
				// being blacklisted takes priority of any key length weaknesses
				issues = "BLACKLISTED"
				blacklisted = true
			}

			fmt.Fprintf(tabWriter, "%d\t%s\t%s\t%s\t\n", length, k.key.Type(), k.Fingerprint(), issues)
		}

		err = tabWriter.Flush()
		if err != nil {
			log.Errorln("Error when flushing tab writer:", err)
		}
		channel.Write([]byte(
			strings.Replace(table.String(), "\n", "\n\r", -1) +
				"\n\r"))

		if blacklisted {
			channel.Write([]byte(blacklistMsg))
		}

		if dsa {
			channel.Write([]byte(dsaMsg))
		}

		if weak {
			channel.Write([]byte(weakMsg))
		}

		reqLock.Lock()
		if agentFwd {
			channel.Write([]byte(agentMsg))
		}
		if x11 {
			channel.Write([]byte(x11Msg))
		}

		// Explicitly close the channel to end the session
		channel.Close()
	}

}

func publicKeyCallback(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	sessions.mu.Lock()
	sessionID := string(conn.SessionID())
	sessions.keys[sessionID] = append(sessions.keys[sessionID], &publicKey{key: key})
	sessions.mu.Unlock()

	// Never succeed a key, or we might not see the next. See KeyboardInteractiveCallback.
	return nil, errors.New("")
}

func keyboardInteractiveCallback(ssh.ConnMetadata, ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
	// keyboard-interactive is tried when all public keys failed, and
	// since it's server-driven we can just pass without user
	// interaction to let the user in once we got all the public keys
	return nil, nil
}

var (
	agentMsg = strings.Replace(`CRITICAL: SSH agent forwarding is enabled; it is dangerous to enable agent forwarding
	  for servers you do not trust as it allows them to log in to other servers as you.

`, "\n", "\n\r", -1)

	blacklistMsg = strings.Replace(`CRITICAL: You are using blacklisted key(s) that are known to be insecure.
          You should replace them immediately.
          See: https://www.debian.org/security/2008/dsa-1576

`, "\n", "\n\r", -1)

	dsaMsg = strings.Replace(`WARNING:  You are using DSA (ssh-dss) key(s), which are no longer supported by
	  default in OpenSSH version 7.0 and above.
          Consider replacing them with a new RSA or ECDSA key.

`, "\n", "\n\r", -1)

	weakMsg = strings.Replace(`WARNING:  You are using RSA key(s) with a length of less than 2048 bits.
          Consider replacing them with a new key of 2048 bits or more.

`, "\n", "\n\r", -1)

	welcomeMsg = strings.Replace(`This server checks your SSH public keys for known or potential
security weaknesses.

For more information, please see:
https://github.com/mattbostock/sshkeycheck

The public keys presented by your SSH client are:

`, "\n", "\n\r", -1)

	x11Msg = strings.Replace(`CRITICAL: X11 forwarding is enabled; it is dangerous to allow X11 forwarding
	  for servers you do not trust as it allows them to access your desktop.

`, "\n", "\n\r", -1)
)
