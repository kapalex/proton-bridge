// Copyright (c) 2023 Proton AG
//
// This file is part of Proton Mail Bridge.
//
// Proton Mail Bridge is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Proton Mail Bridge is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with Proton Mail Bridge.  If not, see <https://www.gnu.org/licenses/>.

package bridge

import (
	"context"
	"fmt"

	"github.com/ProtonMail/gluon/reporter"
	"github.com/ProtonMail/proton-bridge/v3/internal"
	"github.com/ProtonMail/proton-bridge/v3/internal/events"
	"github.com/ProtonMail/proton-bridge/v3/internal/safe"
	"github.com/ProtonMail/proton-bridge/v3/internal/user"
	"github.com/ProtonMail/proton-bridge/v3/internal/vault"
	"github.com/sirupsen/logrus"
)

func (bridge *Bridge) handleUserEvent(ctx context.Context, user *user.User, event events.Event) error {
	switch event := event.(type) {
	case events.UserAddressCreated:
		if err := bridge.handleUserAddressCreated(ctx, user, event); err != nil {
			return fmt.Errorf("failed to handle user address created event: %w", err)
		}

	case events.UserAddressUpdated:
		if err := bridge.handleUserAddressUpdated(ctx, user, event); err != nil {
			return fmt.Errorf("failed to handle user address updated event: %w", err)
		}

	case events.UserAddressDeleted:
		if err := bridge.handleUserAddressDeleted(ctx, user, event); err != nil {
			return fmt.Errorf("failed to handle user address deleted event: %w", err)
		}

	case events.UserRefreshed:
		if err := bridge.handleUserRefreshed(ctx, user); err != nil {
			return fmt.Errorf("failed to handle user refreshed event: %w", err)
		}

	case events.UserDeauth:
		bridge.handleUserDeauth(ctx, user)

	case events.UserBadEvent:
		bridge.handleUserBadEvent(ctx, user, event)

	case events.UncategorizedEventError:
		bridge.handleUncategorizedErrorEvent(event)
	}

	return nil
}

func (bridge *Bridge) handleUserAddressCreated(ctx context.Context, user *user.User, event events.UserAddressCreated) error {
	if user.GetAddressMode() == vault.SplitMode {
		if bridge.imapServer == nil {
			return fmt.Errorf("no imap server instance running")
		}

		gluonID, err := bridge.imapServer.AddUser(ctx, user.NewIMAPConnector(event.AddressID), user.GluonKey())
		if err != nil {
			return fmt.Errorf("failed to add user to IMAP server: %w", err)
		}

		if err := user.SetGluonID(event.AddressID, gluonID); err != nil {
			return fmt.Errorf("failed to set gluon ID: %w", err)
		}
	}

	return nil
}

// GODT-1948: Handle addresses that have been disabled!
func (bridge *Bridge) handleUserAddressUpdated(_ context.Context, user *user.User, _ events.UserAddressUpdated) error {
	switch user.GetAddressMode() {
	case vault.CombinedMode:
		return fmt.Errorf("not implemented")

	case vault.SplitMode:
		return fmt.Errorf("not implemented")
	}

	return nil
}

func (bridge *Bridge) handleUserAddressDeleted(ctx context.Context, user *user.User, event events.UserAddressDeleted) error {
	if user.GetAddressMode() == vault.SplitMode {
		if bridge.imapServer == nil {
			return fmt.Errorf("no imap server instance running")
		}

		gluonID, ok := user.GetGluonID(event.AddressID)
		if !ok {
			return fmt.Errorf("gluon ID not found for address %s", event.AddressID)
		}

		if err := bridge.imapServer.RemoveUser(ctx, gluonID, true); err != nil {
			return fmt.Errorf("failed to remove user from IMAP server: %w", err)
		}

		if err := user.RemoveGluonID(event.AddressID, gluonID); err != nil {
			return fmt.Errorf("failed to remove gluon ID for address: %w", err)
		}
	}

	return nil
}

func (bridge *Bridge) handleUserRefreshed(ctx context.Context, user *user.User) error {
	return safe.RLockRet(func() error {
		if err := bridge.removeIMAPUser(ctx, user, true); err != nil {
			return fmt.Errorf("failed to remove IMAP user: %w", err)
		}

		if err := bridge.addIMAPUser(ctx, user); err != nil {
			return fmt.Errorf("failed to add IMAP user: %w", err)
		}

		return nil
	}, bridge.usersLock)
}

func (bridge *Bridge) handleUserDeauth(ctx context.Context, user *user.User) {
	safe.Lock(func() {
		bridge.logoutUser(ctx, user, false, false)
	}, bridge.usersLock)
}

func (bridge *Bridge) handleUserBadEvent(ctx context.Context, user *user.User, event events.UserBadEvent) {
	safe.Lock(func() {
		reportContext := reporter.Context{
			"user_id":      user.ID(),
			"old_event_id": event.OldEventID,
			"new_event_id": event.NewEventID,
			"event_info":   event.EventInfo,
			"error":        event.Error,
			"error_type":   fmt.Sprintf("%T", internal.ErrCause(event.Error)),
		}

		// blockEventsIMAPandSMTP()

		if doResyc, err := bridge.getBadEventUserFeedback(user.ID()); err != nil || !doResyc {
			if rerr := bridge.reporter.ReportMessageWithContext("Failed to handle event: logout", reportContext); rerr != nil {
				logrus.WithError(rerr).Error("Failed to report failed event handling")
			}

			bridge.logoutUser(ctx, user, true, false)
			return
		}

		if rerr := bridge.reporter.ReportMessageWithContext("Failed to handle event: repair", reportContext); rerr != nil {
			logrus.WithError(rerr).Error("Failed to report event handling failure")
		}

		if syncErr := user.SyncEvent(ctx); syncErr != nil {
			reportContext["error"] = syncErr
			reportContext["error_type"] = fmt.Sprintf("%T", internal.ErrCause(syncErr))
			if rerr := bridge.reporter.ReportMessageWithContext("Failed to handle event: repair failed: logging out", reportContext); rerr != nil {
				logrus.WithError(rerr).Error("Failed to report repair failure")
			}

			bridge.logoutUser(ctx, user, true, false)
			return
		}
	}, bridge.usersLock)
}

func (bridge *Bridge) getBadEventUserFeedback(userID string) (doResyc bool, err error) {
	user, ok := bridge.users[userID]
	if !ok {
		return false, ErrNoSuchUser
	}

	return user.GetBadEventFeedback(), nil
}

func (bridge *Bridge) handleUncategorizedErrorEvent(event events.UncategorizedEventError) {
	if rerr := bridge.reporter.ReportMessageWithContext("Failed to handle due to uncategorized error", reporter.Context{
		"error_type": fmt.Sprintf("%T", internal.ErrCause(event.Error)),
		"error":      event.Error,
	}); rerr != nil {
		logrus.WithError(rerr).Error("Failed to report failed event handling")
	}
}
