package user

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"
	"time"

	"github.com/ProtonMail/gluon/connector"
	"github.com/ProtonMail/gluon/imap"
	"github.com/ProtonMail/gluon/queue"
	"github.com/ProtonMail/proton-bridge/v2/internal/events"
	"github.com/ProtonMail/proton-bridge/v2/internal/safe"
	"github.com/ProtonMail/proton-bridge/v2/internal/try"
	"github.com/ProtonMail/proton-bridge/v2/internal/vault"
	"github.com/bradenaw/juniper/xslices"
	"github.com/emersion/go-smtp"
	"github.com/sirupsen/logrus"
	"gitlab.protontech.ch/go/liteapi"
)

var (
	EventPeriod = 20 * time.Second // nolint:gochecknoglobals
	EventJitter = 20 * time.Second // nolint:gochecknoglobals
)

type User struct {
	vault   *vault.User
	client  *liteapi.Client
	eventCh *queue.QueuedChannel[events.Event]

	apiUser  *safe.Value[liteapi.User]
	apiAddrs *safe.Map[string, liteapi.Address]
	updateCh *safe.Map[string, *queue.QueuedChannel[imap.Update]]

	syncStopCh chan struct{}
	syncLock   try.Group
}

func New(ctx context.Context, encVault *vault.User, client *liteapi.Client, apiUser liteapi.User) (*User, error) {
	// Get the user's API addresses.
	apiAddrs, err := client.GetAddresses(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get addresses: %w", err)
	}

	// Check we can unlock the keyrings.
	if _, _, err := liteapi.Unlock(apiUser, apiAddrs, encVault.KeyPass()); err != nil {
		return nil, fmt.Errorf("failed to unlock user: %w", err)
	}

	// Get the latest event ID.
	if encVault.EventID() == "" {
		eventID, err := client.GetLatestEventID(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get latest event ID: %w", err)
		}

		if err := encVault.SetEventID(eventID); err != nil {
			return nil, fmt.Errorf("failed to set event ID: %w", err)
		}
	}

	// Create update channels for each of the user's addresses.
	// In combined mode, the addresses all share the same update channel.
	updateCh := make(map[string]*queue.QueuedChannel[imap.Update])

	switch encVault.AddressMode() {
	case vault.CombinedMode:
		primaryUpdateCh := queue.NewQueuedChannel[imap.Update](0, 0)

		for _, addr := range apiAddrs {
			updateCh[addr.ID] = primaryUpdateCh
		}

	case vault.SplitMode:
		for _, addr := range apiAddrs {
			updateCh[addr.ID] = queue.NewQueuedChannel[imap.Update](0, 0)
		}
	}

	user := &User{
		vault:   encVault,
		client:  client,
		eventCh: queue.NewQueuedChannel[events.Event](0, 0),

		apiUser:  safe.NewValue(apiUser),
		apiAddrs: safe.NewMapFrom(groupBy(apiAddrs, func(addr liteapi.Address) string { return addr.ID }), sortAddr),
		updateCh: safe.NewMapFrom(updateCh, nil),

		syncStopCh: make(chan struct{}),
	}

	// When we receive an auth object, we update it in the vault.
	// This will be used to authorize the user on the next run.
	user.client.AddAuthHandler(func(auth liteapi.Auth) {
		if err := user.vault.SetAuth(auth.UID, auth.RefreshToken); err != nil {
			logrus.WithError(err).Error("Failed to update auth in vault")
		}
	})

	// When we are deauthorized, we send a deauth event to the event channel.
	// Bridge will react to this event by logging out the user.
	user.client.AddDeauthHandler(func() {
		user.eventCh.Enqueue(events.UserDeauth{
			UserID: user.ID(),
		})
	})

	// TODO: Don't start the event loop until the initial sync has finished!
	eventCh := user.client.NewEventStream(EventPeriod, EventJitter, user.vault.EventID())

	// If we haven't synced yet, do it first.
	// If it fails, we don't start the event loop.
	// Otherwise, begin processing API events, logging any errors that occur.
	go func() {
		if err := <-user.startSync(); err != nil {
			return
		}

		for err := range user.streamEvents(eventCh) {
			logrus.WithError(err).Error("Error while streaming events")
		}
	}()

	return user, nil
}

// ID returns the user's ID.
func (user *User) ID() string {
	return safe.LoadRet(user.apiUser, func(apiUser liteapi.User) string {
		return apiUser.ID
	})
}

// Name returns the user's username.
func (user *User) Name() string {
	return safe.LoadRet(user.apiUser, func(apiUser liteapi.User) string {
		return apiUser.Name
	})
}

// Match matches the given query against the user's username and email addresses.
func (user *User) Match(query string) bool {
	return safe.LoadRet(user.apiUser, func(apiUser liteapi.User) bool {
		if query == apiUser.Name {
			return true
		}

		return user.apiAddrs.HasFunc(func(_ string, addr liteapi.Address) bool {
			return addr.Email == query
		})
	})
}

// Emails returns all the user's email addresses via the callback.
func (user *User) Emails() []string {
	return safe.MapValuesRet(user.apiAddrs, func(apiAddrs []liteapi.Address) []string {
		return xslices.Map(apiAddrs, func(addr liteapi.Address) string {
			return addr.Email
		})
	})
}

// GetAddressMode returns the user's current address mode.
func (user *User) GetAddressMode() vault.AddressMode {
	return user.vault.AddressMode()
}

// SetAddressMode sets the user's address mode.
func (user *User) SetAddressMode(ctx context.Context, mode vault.AddressMode) error {
	user.stopSync()
	user.lockSync()
	defer user.unlockSync()

	user.updateCh.Values(func(updateCh []*queue.QueuedChannel[imap.Update]) {
		for _, updateCh := range xslices.Unique(updateCh) {
			updateCh.Close()
		}
	})

	updateCh := make(map[string]*queue.QueuedChannel[imap.Update])

	switch mode {
	case vault.CombinedMode:
		primaryUpdateCh := queue.NewQueuedChannel[imap.Update](0, 0)

		user.apiAddrs.IterKeys(func(addrID string) {
			updateCh[addrID] = primaryUpdateCh
		})

	case vault.SplitMode:
		user.apiAddrs.IterKeys(func(addrID string) {
			updateCh[addrID] = queue.NewQueuedChannel[imap.Update](0, 0)
		})
	}

	user.updateCh = safe.NewMapFrom(updateCh, nil)

	if err := user.vault.SetAddressMode(mode); err != nil {
		return fmt.Errorf("failed to set address mode: %w", err)
	}

	if err := user.vault.ClearSyncStatus(); err != nil {
		return fmt.Errorf("failed to clear sync status: %w", err)
	}

	go func() {
		if err := <-user.startSync(); err != nil {
			logrus.WithError(err).Error("Failed to sync after setting address mode")
		}
	}()

	return nil
}

// GetGluonIDs returns the users gluon IDs.
func (user *User) GetGluonIDs() map[string]string {
	return user.vault.GetGluonIDs()
}

// GetGluonID returns the gluon ID for the given address, if present.
func (user *User) GetGluonID(addrID string) (string, bool) {
	gluonID, ok := user.vault.GetGluonIDs()[addrID]
	if !ok {
		return "", false
	}

	return gluonID, true
}

// SetGluonID sets the gluon ID for the given address.
func (user *User) SetGluonID(addrID, gluonID string) error {
	return user.vault.SetGluonID(addrID, gluonID)
}

// GluonKey returns the user's gluon key from the vault.
func (user *User) GluonKey() []byte {
	return user.vault.GluonKey()
}

// BridgePass returns the user's bridge password, used for authentication over SMTP and IMAP.
func (user *User) BridgePass() []byte {
	return hexEncode(user.vault.BridgePass())
}

// UsedSpace returns the total space used by the user on the API.
func (user *User) UsedSpace() int {
	return safe.LoadRet(user.apiUser, func(apiUser liteapi.User) int {
		return apiUser.UsedSpace
	})
}

// MaxSpace returns the amount of space the user can use on the API.
func (user *User) MaxSpace() int {
	return safe.LoadRet(user.apiUser, func(apiUser liteapi.User) int {
		return apiUser.MaxSpace
	})
}

// GetEventCh returns a channel which notifies of events happening to the user (such as deauth, address change)
func (user *User) GetEventCh() <-chan events.Event {
	return user.eventCh.GetChannel()
}

// NewIMAPConnector returns an IMAP connector for the given address.
// If not in split mode, this must be the primary address.
func (user *User) NewIMAPConnector(addrID string) connector.Connector {
	return newIMAPConnector(user, addrID)
}

// NewIMAPConnectors returns IMAP connectors for each of the user's addresses.
// In combined mode, this is just the user's primary address.
// In split mode, this is all the user's addresses.
func (user *User) NewIMAPConnectors() (map[string]connector.Connector, error) {
	imapConn := make(map[string]connector.Connector)

	switch user.vault.AddressMode() {
	case vault.CombinedMode:
		user.apiAddrs.Index(0, func(addrID string, _ liteapi.Address) {
			imapConn[addrID] = newIMAPConnector(user, addrID)
		})

	case vault.SplitMode:
		user.apiAddrs.IterKeys(func(addrID string) {
			imapConn[addrID] = newIMAPConnector(user, addrID)
		})
	}

	return imapConn, nil
}

// NewSMTPSession returns an SMTP session for the user.
func (user *User) NewSMTPSession(email string, password []byte) (smtp.Session, error) {
	if _, err := user.checkAuth(email, password); err != nil {
		return nil, err
	}

	return newSMTPSession(user, email)
}

// OnStatusUp is called when the connection goes up.
func (user *User) OnStatusUp() {
	go func() {
		logrus.Info("Connection up, checking if sync is needed")

		if err := <-user.startSync(); err != nil {
			logrus.WithError(err).Error("Failed to sync on status up")
		}
	}()
}

// OnStatusDown is called when the connection goes down.
func (user *User) OnStatusDown() {
	logrus.Info("Connection down, aborting any ongoing syncs")

	user.stopSync()
}

// Logout logs the user out from the API.
// If withVault is true, the user's vault is also cleared.
func (user *User) Logout(ctx context.Context) error {
	if err := user.client.AuthDelete(ctx); err != nil {
		return fmt.Errorf("failed to delete auth: %w", err)
	}

	if err := user.vault.Clear(); err != nil {
		return fmt.Errorf("failed to clear vault: %w", err)
	}

	return nil
}

// Close closes ongoing connections and cleans up resources.
func (user *User) Close() error {
	// Cancel ongoing syncs.
	user.stopSync()

	// Wait for ongoing syncs to stop.
	user.waitSync()

	// Close the user's API client.
	if err := user.client.Close(); err != nil {
		logrus.WithError(err).Error("Failed to close API client")
	}

	// Close the user's update channels.
	user.updateCh.Values(func(updateCh []*queue.QueuedChannel[imap.Update]) {
		for _, updateCh := range xslices.Unique(updateCh) {
			updateCh.Close()
		}
	})

	// Close the user's notify channel.
	user.eventCh.Close()

	// Close the user's vault.
	if err := user.vault.Close(); err != nil {
		logrus.WithError(err).Error("Failed to close vault")
	}

	return nil
}

func (user *User) checkAuth(email string, password []byte) (string, error) {
	dec, err := hexDecode(password)
	if err != nil {
		return "", fmt.Errorf("failed to decode password: %w", err)
	}

	if subtle.ConstantTimeCompare(user.vault.BridgePass(), dec) != 1 {
		return "", fmt.Errorf("invalid password")
	}

	return safe.MapValuesRetErr(user.apiAddrs, func(apiAddrs []liteapi.Address) (string, error) {
		for _, addr := range apiAddrs {
			if addr.Email == strings.ToLower(email) {
				return addr.ID, nil
			}
		}

		return "", fmt.Errorf("invalid email")
	})
}

// streamEvents begins streaming API events for the user.
// When we receive an API event, we attempt to handle it.
// If successful, we update the event ID in the vault.
func (user *User) streamEvents(eventCh <-chan liteapi.Event) <-chan error {
	errCh := make(chan error)

	go func() {
		defer close(errCh)

		for event := range eventCh {
			if err := user.handleAPIEvent(context.Background(), event); err != nil {
				errCh <- fmt.Errorf("failed to handle API event: %w", err)
			} else if err := user.vault.SetEventID(event.EventID); err != nil {
				errCh <- fmt.Errorf("failed to update event ID: %w", err)
			}
		}
	}()

	return errCh
}

// startSync begins a startSync for the user.
func (user *User) startSync() <-chan error {
	if user.vault.SyncStatus().IsComplete() {
		logrus.Debug("Already synced, skipping")
		return nil
	}

	errCh := make(chan error)

	user.syncLock.GoTry(func(ok bool) {
		defer close(errCh)

		if !ok {
			logrus.Debug("Sync already in progress, skipping")
			return
		}

		ctx, cancel := contextWithStopCh(context.Background(), user.syncStopCh)
		defer cancel()

		user.eventCh.Enqueue(events.SyncStarted{
			UserID: user.ID(),
		})

		if err := user.sync(ctx); err != nil {
			user.eventCh.Enqueue(events.SyncFailed{
				UserID: user.ID(),
				Err:    err,
			})

			errCh <- err
		} else {
			user.eventCh.Enqueue(events.SyncFinished{
				UserID: user.ID(),
			})
		}
	})

	return errCh
}

// AbortSync aborts any ongoing sync.
// TODO: Should probably be done automatically when one of the user's IMAP connectors is closed.
func (user *User) stopSync() {
	select {
	case user.syncStopCh <- struct{}{}:
		logrus.Debug("Sent sync abort signal")

	default:
		logrus.Debug("No sync to abort")
	}
}

// lockSync prevents a new sync from starting.
func (user *User) lockSync() {
	user.syncLock.Lock()
}

// unlockSync allows a new sync to start.
func (user *User) unlockSync() {
	user.syncLock.Unlock()
}

// waitSync waits for any ongoing sync to finish.
func (user *User) waitSync() {
	user.syncLock.Wait()
}
