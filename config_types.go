package main

type StatusnookConfigSlackNotificationChannel struct {
	WebhookURL string `json:"webhookURL" yaml:"webhook-url"`
}

type StatusnookConfigSMTPNotificationChannel struct {
	Host     string            `json:"host" yaml:"host"`
	Port     int               `json:"port" yaml:"port"`
	Username string            `json:"username" yaml:"username"`
	Password string            `json:"password" yaml:"password"`
	From     string            `json:"from" yaml:"from"`
	Headers  map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Misc     map[string]string `json:"misc,omitempty" yaml:"misc,omitempty"`
}

type StatusnookConfigMonitor struct {
	Name                 string            `json:"name" yaml:"name"`
	URL                  string            `json:"url" yaml:"url"`
	Method               string            `json:"method" yaml:"method"`
	Frequency            int               `json:"frequency" yaml:"frequency"`
	Timeout              int               `json:"timeout" yaml:"timeout"`
	Attempts             int               `json:"attempts" yaml:"attempts"`
	RequestHeaders       map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	RequestBody          any               `json:"body,omitempty" yaml:"body,omitempty"`
	NotificationChannels []string          `json:"notification-channels,omitempty" yaml:"notification-channels,omitempty"`
	MailGroups           []string          `json:"mail-groups,omitempty" yaml:"mail-groups,omitempty"`
}

type StatusnookConfigGeneralSettings struct {
	Name string `json:"name" yaml:"name"`
}

type StatusnookConfigService struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

type StatusnookConfigAlertNotificationSettings struct {
	EmailNotificationChannel string `json:"email-notification-channel,omitempty" yaml:"email-notification-channel,omitempty"`
	ManagedSubscriptions     bool   `json:"managed-subscriptions,omitempty" yaml:"managed-subscriptions,omitempty"`
	SlackClientSecret        string `json:"slack-client-secret,omitempty" yaml:"slack-client-secret,omitempty"`
	SlackInstallURL          string `json:"slack-install-url,omitempty" yaml:"slack-install-url,omitempty"`
}

type StatusnookConfigMailGroup struct {
	Name        string   `json:"name" yaml:"name"`
	Members     []string `json:"members,omitempty" yaml:"members,omitempty"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
}

type StatusnookConfig struct {
	GeneralSettings           StatusnookConfigGeneralSettings           `json:"general-settings" yaml:"general-settings"`
	MailGroups                map[string]StatusnookConfigMailGroup      `json:"mail-groups,omitempty" yaml:"mail-groups,omitempty"`
	NotificationChannels      map[string]map[string]any                 `json:"notification-channels,omitempty" yaml:"notification-channels,omitempty"`
	Monitors                  map[string]StatusnookConfigMonitor        `json:"monitors,omitempty" yaml:"monitors,omitempty"`
	Services                  map[string]StatusnookConfigService        `json:"services,omitempty" yaml:"services,omitempty"`
	AlertNotificationSettings StatusnookConfigAlertNotificationSettings `json:"alert-notification-settings,omitempty" yaml:"alert-notification-settings,omitempty"`
	Rename                    map[string]string                         `json:"rename,omitempty" yaml:"rename,omitempty"`
}
