package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type AccountState struct {
	Provider      string    `json:"provider"`
	Status        string    `json:"status"`
	LastCheck     time.Time `json:"last_check"`
	NextCheck     time.Time `json:"next_check"`
	Quota         Quota     `json:"quota"`
	PreviousCheck time.Time `json:"previous_check"`
	PreviousReset time.Time `json:"previous_reset"`
	PreviousUsed  float64   `json:"previous_used"`
	IdleSince     time.Time `json:"idle_since"`
	LastAttempt   time.Time `json:"last_attempt"`
	NextAttempt   time.Time `json:"next_attempt"`
	LastSuccess   time.Time `json:"last_success"`
	VerifiedReset time.Time `json:"verified_reset"`
	Attempts      int       `json:"attempts"`
	Successes     int       `json:"successes"`
	Failures      int       `json:"failures"`
	LastError     string    `json:"last_error,omitempty"`
}

type State struct {
	Version  int                      `json:"version"`
	Accounts map[string]*AccountState `json:"accounts"`
}

type Store struct {
	path string
	lock *os.File
}

func openStore(path string) (*Store, State, error) {
	initial := State{Version: 1, Accounts: map[string]*AccountState{}}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, initial, errors.New("cannot create state directory")
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, initial, errors.New("cannot open state lock")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, initial, errors.New("another keeper owns this state path")
	}
	s := &Store{path: path, lock: lock}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, initial, nil
	}
	if err != nil {
		_ = s.Close()
		return nil, initial, errors.New("cannot read state")
	}
	if len(raw) > 4<<20 || json.Unmarshal(raw, &initial) != nil || initial.Version != 1 || initial.Accounts == nil {
		_ = s.Close()
		return nil, State{}, errors.New("invalid state; refusing to reset duplicate protection")
	}
	for key, a := range initial.Accounts {
		if key == "" || a == nil {
			_ = s.Close()
			return nil, State{}, errors.New("invalid account state")
		}
	}
	return s, initial, nil
}

func (s *Store) Save(state State) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return errors.New("cannot encode state")
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".window-state-*")
	if err != nil {
		return errors.New("cannot create state temporary file")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, s.path)
	}
	if err == nil {
		d, e := os.Open(filepath.Dir(s.path))
		if e != nil {
			err = e
		} else {
			err = d.Sync()
			_ = d.Close()
		}
	}
	if err != nil {
		return errors.New("state persistence failed; sending is suspended")
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}
