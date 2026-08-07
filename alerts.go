package main

import (
	"time"
)

type UnsentAlertNotification struct {
	AlertNotificationID int
	Destination         string
	Content             string
	Type                string
	AlertMessageID      int
	AlertTitle          string
	AlertType           string
	AlertSeverity       string
	AlertServices       string
}

type StatusnookConfigAlertNotificationSettings struct {
	EmailNotificationChannel string `json:"email-notification-channel,omitempty" yaml:"email-notification-channel,omitempty"`
	ManagedSubscriptions     bool   `json:"managed-subscriptions,omitempty" yaml:"managed-subscriptions,omitempty"`
	SlackClientSecret        string `json:"slack-client-secret,omitempty" yaml:"slack-client-secret,omitempty"`
	SlackInstallURL          string `json:"slack-install-url,omitempty" yaml:"slack-install-url,omitempty"`
}

type AlertSubscription struct {
	ID          int
	Type        string
	Destination string
	Meta        string
	Active      bool
}

type AlertListing struct {
	ID        int
	Title     string
	AlertType string
	Severity  string
	CreatedAt *time.Time
	EndedAt   *time.Time
}
