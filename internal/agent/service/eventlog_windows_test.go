//go:build windows

package service

import (
	"errors"
	"strings"
	"testing"
)

func TestEnsureWindowsEventSourceLifecycle(t *testing.T) {
	tests := []struct {
		name        string
		store       *fakeWindowsEventSourceStore
		wantCreated bool
		wantError   string
		wantInstall int
	}{
		{
			name:        "首次注册",
			store:       &fakeWindowsEventSourceStore{},
			wantCreated: true,
			wantInstall: 1,
		},
		{
			name:  "精确受管 Source 可复用",
			store: &fakeWindowsEventSourceStore{exists: true, managed: true},
		},
		{
			name:      "外来同名 Source 拒绝覆盖",
			store:     &fakeWindowsEventSourceStore{exists: true},
			wantError: "refusing to overwrite",
		},
		{
			name:      "检查失败向上传播",
			store:     &fakeWindowsEventSourceStore{inspectErr: errors.New("registry denied")},
			wantError: "registry denied",
		},
		{
			name:        "注册失败向上传播",
			store:       &fakeWindowsEventSourceStore{installErr: errors.New("install denied")},
			wantError:   "install denied",
			wantInstall: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			created, err := ensureWindowsEventSource(test.store)
			if created != test.wantCreated {
				t.Fatalf("ensureWindowsEventSource() created = %t, want %t", created, test.wantCreated)
			}
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("ensureWindowsEventSource() error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("ensureWindowsEventSource() error = %v, want substring %q", err, test.wantError)
			}
			if test.store.installCalls != test.wantInstall {
				t.Fatalf("install calls = %d, want %d", test.store.installCalls, test.wantInstall)
			}
		})
	}
}

func TestInstallWindowsEventSourceRollsBackOnlyNewSource(t *testing.T) {
	t.Run("标准值写入失败回滚本次新建 Source", func(t *testing.T) {
		key := &fakeWindowsEventSourceRegistryKey{expandErr: errors.New("message file write failed")}
		removeCalls := 0
		err := installWindowsEventSource(
			windowsEventLogSource,
			func(string) (windowsEventSourceRegistryKey, bool, error) { return key, false, nil },
			func(string) error { removeCalls++; return nil },
		)
		if err == nil || !strings.Contains(err.Error(), "message file write failed") {
			t.Fatalf("installWindowsEventSource() error = %v", err)
		}
		if key.closeCalls != 1 || removeCalls != 1 {
			t.Fatalf("rollback close/remove calls = (%d, %d), want (1, 1)", key.closeCalls, removeCalls)
		}
	})

	t.Run("并发出现的同名 Source 拒绝接管且不删除", func(t *testing.T) {
		key := &fakeWindowsEventSourceRegistryKey{}
		removeCalls := 0
		err := installWindowsEventSource(
			windowsEventLogSource,
			func(string) (windowsEventSourceRegistryKey, bool, error) { return key, true, nil },
			func(string) error { removeCalls++; return nil },
		)
		if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
			t.Fatalf("installWindowsEventSource() error = %v", err)
		}
		if key.closeCalls != 1 || removeCalls != 0 {
			t.Fatalf("foreign Source close/remove calls = (%d, %d), want (1, 0)", key.closeCalls, removeCalls)
		}
	})
}

func TestRemoveManagedWindowsEventSourceLifecycle(t *testing.T) {
	tests := []struct {
		name       string
		store      *fakeWindowsEventSourceStore
		wantError  string
		wantRemove int
	}{
		{name: "不存在时幂等", store: &fakeWindowsEventSourceStore{}},
		{
			name:       "只删除受管 Source",
			store:      &fakeWindowsEventSourceStore{exists: true, managed: true},
			wantRemove: 1,
		},
		{
			name:      "外来同名 Source 拒绝删除",
			store:     &fakeWindowsEventSourceStore{exists: true},
			wantError: "refusing to remove",
		},
		{
			name:       "删除失败向上传播",
			store:      &fakeWindowsEventSourceStore{exists: true, managed: true, removeErr: errors.New("remove denied")},
			wantError:  "remove denied",
			wantRemove: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := removeManagedWindowsEventSource(test.store)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("removeManagedWindowsEventSource() error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("removeManagedWindowsEventSource() error = %v, want substring %q", err, test.wantError)
			}
			if test.store.removeCalls != test.wantRemove {
				t.Fatalf("remove calls = %d, want %d", test.store.removeCalls, test.wantRemove)
			}
		})
	}
}

func TestRequireManagedWindowsEventSourceRejectsFallbackSource(t *testing.T) {
	for _, test := range []struct {
		name      string
		store     *fakeWindowsEventSourceStore
		wantError bool
	}{
		{name: "受管 Source", store: &fakeWindowsEventSourceStore{exists: true, managed: true}},
		{name: "Source 缺失", store: &fakeWindowsEventSourceStore{}, wantError: true},
		{name: "Source 被修改", store: &fakeWindowsEventSourceStore{exists: true}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requireManagedWindowsEventSource(test.store)
			if (err != nil) != test.wantError {
				t.Fatalf("requireManagedWindowsEventSource() error = %v, wantError=%t", err, test.wantError)
			}
		})
	}
}

func TestWindowsEventLogWriterMapsJSONLevelsAndPreservesPayload(t *testing.T) {
	log := &fakeWindowsEventLogger{}
	writer, err := openWindowsEventLogWriter(windowsEventLogSource, func(source string) (windowsEventLogger, error) {
		if source != windowsEventLogSource {
			t.Fatalf("source = %q", source)
		}
		return log, nil
	})
	if err != nil {
		t.Fatalf("openWindowsEventLogWriter() error = %v", err)
	}

	records := []struct {
		payload string
		kind    string
		id      uint32
	}{
		{`{"level":"debug","event":"debug_event"}` + "\n", "info", windowsEventLogInformationID},
		{`{"level":"info","event":"info_event"}` + "\n", "info", windowsEventLogInformationID},
		{`{"level":"warn","event":"warn_event"}` + "\n", "warning", windowsEventLogWarningID},
		{`{"level":"error","event":"error_event"}` + "\n", "error", windowsEventLogErrorID},
	}
	for _, record := range records {
		if written, err := writer.Write([]byte(record.payload)); err != nil || written != len(record.payload) {
			t.Fatalf("Write(%q) = (%d, %v)", record.payload, written, err)
		}
	}
	if len(log.records) != len(records) {
		t.Fatalf("event records = %d, want %d", len(log.records), len(records))
	}
	for index, want := range records {
		got := log.records[index]
		if got.kind != want.kind || got.id != want.id || got.message != strings.TrimSuffix(want.payload, "\n") {
			t.Fatalf("record %d = %#v, want kind=%q id=%d payload=%q", index, got, want.kind, want.id, want.payload)
		}
	}
}

func TestWindowsEventLogWriterPropagatesOpenWriteAndCloseFailures(t *testing.T) {
	openFailure := errors.New("open failed")
	if _, err := openWindowsEventLogWriter(windowsEventLogSource, func(string) (windowsEventLogger, error) {
		return nil, openFailure
	}); !errors.Is(err, openFailure) {
		t.Fatalf("open error = %v, want %v", err, openFailure)
	}

	writeFailure := errors.New("report failed")
	closeFailure := errors.New("close failed")
	log := &fakeWindowsEventLogger{writeErr: writeFailure, closeErr: closeFailure}
	writer, err := openWindowsEventLogWriter(windowsEventLogSource, func(string) (windowsEventLogger, error) {
		return log, nil
	})
	if err != nil {
		t.Fatalf("openWindowsEventLogWriter() error = %v", err)
	}
	if _, err := writer.Write([]byte(`{"level":"error","event":"failed"}` + "\n")); !errors.Is(err, writeFailure) {
		t.Fatalf("write error = %v, want %v", err, writeFailure)
	}
	select {
	case failure := <-writer.Failures():
		if !errors.Is(failure, writeFailure) {
			t.Fatalf("failure signal = %v, want %v", failure, writeFailure)
		}
	default:
		t.Fatal("Event Log writer did not publish its write failure")
	}
	if err := writer.Close(); !errors.Is(err, closeFailure) {
		t.Fatalf("close error = %v, want %v", err, closeFailure)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if _, err := writer.Write([]byte(`{"level":"info"}` + "\n")); err == nil {
		t.Fatal("Write() after Close succeeded")
	}
}

func TestWindowsEventLogWriterRejectsInvalidRecord(t *testing.T) {
	writer, err := openWindowsEventLogWriter(windowsEventLogSource, func(string) (windowsEventLogger, error) {
		return &fakeWindowsEventLogger{}, nil
	})
	if err != nil {
		t.Fatalf("openWindowsEventLogWriter() error = %v", err)
	}
	for _, payload := range []string{
		"not-json\n",
		`{"level":"notice"}` + "\n",
		`{"level":"info"}` + "\n" + `{"level":"error"}` + "\n",
	} {
		if _, err := writer.Write([]byte(payload)); err == nil {
			t.Fatalf("Write(%q) succeeded", payload)
		}
	}
}

type fakeWindowsEventSourceStore struct {
	exists       bool
	managed      bool
	inspectErr   error
	installErr   error
	removeErr    error
	installCalls int
	removeCalls  int
}

type fakeWindowsEventSourceRegistryKey struct {
	expandErr  error
	closeCalls int
}

func (*fakeWindowsEventSourceRegistryKey) SetDWordValue(string, uint32) error { return nil }

func (key *fakeWindowsEventSourceRegistryKey) SetExpandStringValue(string, string) error {
	return key.expandErr
}

func (*fakeWindowsEventSourceRegistryKey) SetStringValue(string, string) error { return nil }

func (key *fakeWindowsEventSourceRegistryKey) Close() error {
	key.closeCalls++
	return nil
}

func (store *fakeWindowsEventSourceStore) inspect(string) (bool, bool, error) {
	return store.exists, store.managed, store.inspectErr
}

func (store *fakeWindowsEventSourceStore) install(string) error {
	store.installCalls++
	return store.installErr
}

func (store *fakeWindowsEventSourceStore) remove(string) error {
	store.removeCalls++
	return store.removeErr
}

type windowsEventRecord struct {
	kind    string
	id      uint32
	message string
}

type fakeWindowsEventLogger struct {
	records  []windowsEventRecord
	writeErr error
	failAt   int
	writes   int
	closeErr error
}

func (log *fakeWindowsEventLogger) Info(id uint32, message string) error {
	return log.record("info", id, message)
}

func (log *fakeWindowsEventLogger) Warning(id uint32, message string) error {
	return log.record("warning", id, message)
}

func (log *fakeWindowsEventLogger) Error(id uint32, message string) error {
	return log.record("error", id, message)
}

func (log *fakeWindowsEventLogger) record(kind string, id uint32, message string) error {
	log.records = append(log.records, windowsEventRecord{kind: kind, id: id, message: message})
	log.writes++
	if log.writeErr != nil && (log.failAt == 0 || log.writes >= log.failAt) {
		return log.writeErr
	}
	return nil
}

func (log *fakeWindowsEventLogger) Close() error {
	return log.closeErr
}
