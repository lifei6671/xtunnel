//go:build linux

package bootstrap

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAdminPasswordLinuxRestoresTTYAndPropagatesErrors(t *testing.T) {
	for _, scenario := range []string{"success", "empty", "prompt", "finish", "restore"} {
		t.Run(scenario, func(t *testing.T) {
			master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { master.Close() })
			if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
				t.Fatal(err)
			}
			number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
			if err != nil {
				t.Fatal(err)
			}
			input, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { input.Close() })
			before, err := unix.IoctlGetTermios(int(input.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("test prompt output failure")
			writer := linuxPasswordPromptWriter(func(data []byte) (int, error) {
				if string(data) == "Admin password: " {
					mode, err := unix.IoctlGetTermios(int(input.Fd()), unix.TCGETS)
					if err != nil || mode.Lflag&unix.ECHO != 0 {
						t.Fatal("password echo was not disabled")
					}
					if scenario == "restore" {
						if err := input.Close(); err != nil {
							t.Fatal(err)
						}
						return 0, failure
					}
					if scenario == "prompt" {
						return 0, failure
					}
					value := "test-only-password\n"
					if scenario == "empty" {
						value = "\n"
					}
					if _, err := master.WriteString(value); err != nil {
						t.Fatal(err)
					}
				} else if scenario == "finish" {
					return 0, failure
				}
				return len(data), nil
			})
			password, err := readAdminPasswordFromTTY(input, writer)
			if scenario == "success" {
				if err != nil || password != "test-only-password" {
					t.Fatal("password read failed")
				}
			} else if err == nil || password != "" {
				t.Fatal("failed input returned a password or no error")
			}
			if scenario == "prompt" || scenario == "finish" || scenario == "restore" {
				if !errors.Is(err, failure) {
					t.Fatal("prompt error chain was lost")
				}
			}
			if scenario == "restore" {
				if !strings.Contains(err.Error(), "restore TTY settings") {
					t.Fatal("restore error was lost")
				}
				return
			}
			after, err := unix.IoctlGetTermios(int(input.Fd()), unix.TCGETS)
			if err != nil || *after != *before {
				t.Fatal("TTY settings were not restored")
			}
		})
	}
}

type linuxPasswordPromptWriter func([]byte) (int, error)

func (writer linuxPasswordPromptWriter) Write(data []byte) (int, error) { return writer(data) }
