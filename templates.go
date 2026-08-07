package main

// The page and message templates live under templates/ as .html and .json
// files rather than as backtick constants inside their handlers. That is
// where an editor, a linter and a diff can all see them as what they are --
// and it is what kept main.go from being readable at 19,869 lines.
//
// Embedded per file, not through an embed.FS lookup: a renamed or deleted
// template is then a build error rather than a blank page at request time.

import _ "embed"

//go:embed templates/parse_tmpl_root_tmpl.html
var parseTmplRootTmpl string

//go:embed templates/send_monitor_alert_email_down_markup.html
var sendMonitorAlertEmailDownMarkup string

//go:embed templates/send_monitor_alert_email_up_markup.html
var sendMonitorAlertEmailUpMarkup string

//go:embed templates/send_monitor_alert_slack_down_markup.html
var sendMonitorAlertSlackDownMarkup string

//go:embed templates/send_monitor_alert_slack_up_markup.html
var sendMonitorAlertSlackUpMarkup string

//go:embed templates/notification_loop_text.json
var notificationLoopText string

//go:embed templates/notification_loop.html
var notificationLoopMarkup string

//go:embed templates/post_subscribe_email.html
var postSubscribeEmailMarkup string

//go:embed templates/get_subscribe_email_confirm.html
var getSubscribeEmailConfirmMarkup string

//go:embed templates/get_unsubscribe.html
var getUnsubscribeMarkup string

//go:embed templates/post_unsubscribe.html
var postUnsubscribeMarkup string

//go:embed templates/post_resubscribe.html
var postResubscribeMarkup string

//go:embed templates/get_invitation.html
var getInvitationMarkup string

//go:embed templates/index.html
var indexMarkup string

//go:embed templates/history.html
var historyMarkup string

//go:embed templates/get_login.html
var getLoginMarkup string

//go:embed templates/alerts.html
var alertsMarkup string

//go:embed templates/monitors.html
var monitorsMarkup string

//go:embed templates/get_monitor.html
var getMonitorMarkup string

//go:embed templates/get_monitor_all_logs.html
var getMonitorAllLogsMarkup string

//go:embed templates/get_monitor_poll.html
var getMonitorPollMarkup string

//go:embed templates/get_edit_monitor.html
var getEditMonitorMarkup string

//go:embed templates/get_create_monitor.html
var getCreateMonitorMarkup string

//go:embed templates/get_alert_notifications.html
var getAlertNotificationsMarkup string

//go:embed templates/get_alert.html
var getAlertMarkup string

//go:embed templates/get_create_alert.html
var getCreateAlertMarkup string

//go:embed templates/get_edit_alert.html
var getEditAlertMarkup string

//go:embed templates/get_add_alert_message.html
var getAddAlertMessageMarkup string

//go:embed templates/get_edit_alert_message.html
var getEditAlertMessageMarkup string

//go:embed templates/services.html
var servicesMarkup string

//go:embed templates/get_create_service.html
var getCreateServiceMarkup string

//go:embed templates/get_edit_service.html
var getEditServiceMarkup string

//go:embed templates/notifications.html
var notificationsMarkup string

//go:embed templates/get_create_notification.html
var getCreateNotificationMarkup string

//go:embed templates/get_edit_notification.html
var getEditNotificationMarkup string

//go:embed templates/get_create_mail_group.html
var getCreateMailGroupMarkup string

//go:embed templates/get_edit_mail_group.html
var getEditMailGroupMarkup string

//go:embed templates/update.html
var updateMarkup string

//go:embed templates/update_check.html
var updateCheckMarkup string

//go:embed templates/after_update.html
var afterUpdateMarkup string

//go:embed templates/post_update.html
var postUpdateMarkup string

//go:embed templates/get_settings.html
var getSettingsMarkup string

//go:embed templates/get_edit_user.html
var getEditUserMarkup string

//go:embed templates/get_config_settings.html
var getConfigSettingsMarkup string

//go:embed templates/get_setup_domain.html
var getSetupDomainMarkup string

//go:embed templates/get_setup_account.html
var getSetupAccountMarkup string

//go:embed templates/get_setup_name.html
var getSetupNameMarkup string

