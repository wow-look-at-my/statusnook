package main

import (
	"log"
	"net/http"
)

func notifications(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("notifications.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	channels, err := listNotificationChannels(tx, listNotificationsOptions{})
	if err != nil {
		log.Printf("notifications.listNotificationChannels: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		log.Printf("notifications.listMailGroups: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("notifications.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	const markup = `
		{{define "title"}}Notifications{{end}}
		{{define "body"}}
			<div class="admin-nav-header admin-nav-header--notifications">
				<div>
					<h2>Notifications</h2>
				</div>
			</div>

			<div class="notifications-container">
				<div class="notification-channels-header">
					<h2>Channels</h2>

					{{if not .Ctx.ConfigFile}}
						<div>
							<a href="/admin/notifications/create" hx-boost="true">
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
									<path d="M10.75 4.75a.75.75 0 00-1.5 0v4.5h-4.5a.75.75 0 000 1.5h4.5v4.5a.75.75 0 001.5 0v-4.5h4.5a.75.75 0 000-1.5h-4.5v-4.5z" />
								</svg>
							</a>
						</div>
					{{end}}
				</div>

				<div style="margin-bottom: 5.0rem;">
					{{if eq (len .Notifications) 0}}
						<div class="entity-empty-state">
							<div class="icon">
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
									<path fill-rule="evenodd" d="M10 2a6 6 0 0 0-6 6c0 1.887-.454 3.665-1.257 5.234a.75.75 0 0 0 .515 1.076 32.91 32.91 0 0 0 3.256.508 3.5 3.5 0 0 0 6.972 0 32.903 32.903 0 0 0 3.256-.508.75.75 0 0 0 .515-1.076A11.448 11.448 0 0 1 16 8a6 6 0 0 0-6-6ZM8.05 14.943a33.54 33.54 0 0 0 3.9 0 2 2 0 0 1-3.9 0Z" clip-rule="evenodd" />
								</svg>
							</div>
							<span>Add your first notification channel</span>
							{{if not .Ctx.ConfigFile}}
								<a class="action" href="/admin/notifications/create" hx-boost="true">Add channel</a>
							{{else}}
								<a class="action" href="/admin/settings#config-form" hx-boost="true">Go to settings</a>
							{{end}}
						</div>
					{{else}}
						<div class="notifications-list">
							{{range $notification := .Notifications}}
								{{if $.Ctx.ConfigFile}}
								<a href="/admin/notifications/{{$notification.ID}}/view" hx-boost="true">
								{{else}}
								<div>
								{{end}}
									<div>
										{{if eq $notification.Type "smtp"}}
											<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
												<path d="M3 4a2 2 0 00-2 2v1.161l8.441 4.221a1.25 1.25 0 001.118 0L19 7.162V6a2 2 0 00-2-2H3z" />
												<path d="M19 8.839l-7.77 3.885a2.75 2.75 0 01-2.46 0L1 8.839V14a2 2 0 002 2h14a2 2 0 002-2V8.839z" />
											</svg>
										{{else if eq $notification.Type "slack"}}
											<svg viewBox="0 0 124 124" fill="none" xmlns="http://www.w3.org/2000/svg">
												<path d="M26.3996 78.2003C26.3996 85.3003 20.5996 91.1003 13.4996 91.1003C6.39961 91.1003 0.599609 85.3003 0.599609 78.2003C0.599609 71.1003 6.39961 65.3003 13.4996 65.3003H26.3996V78.2003Z" fill="#E01E5A"/>
												<path d="M32.9004 78.2003C32.9004 71.1003 38.7004 65.3003 45.8004 65.3003C52.9004 65.3003 58.7004 71.1003 58.7004 78.2003V110.5C58.7004 117.6 52.9004 123.4 45.8004 123.4C38.7004 123.4 32.9004 117.6 32.9004 110.5V78.2003Z" fill="#E01E5A"/>
												<path d="M45.8004 26.4001C38.7004 26.4001 32.9004 20.6001 32.9004 13.5001C32.9004 6.4001 38.7004 0.600098 45.8004 0.600098C52.9004 0.600098 58.7004 6.4001 58.7004 13.5001V26.4001H45.8004Z" fill="#36C5F0"/>
												<path d="M45.7996 32.8999C52.8996 32.8999 58.6996 38.6999 58.6996 45.7999C58.6996 52.8999 52.8996 58.6999 45.7996 58.6999H13.4996C6.39961 58.6999 0.599609 52.8999 0.599609 45.7999C0.599609 38.6999 6.39961 32.8999 13.4996 32.8999H45.7996Z" fill="#36C5F0"/>
												<path d="M97.5996 45.7999C97.5996 38.6999 103.4 32.8999 110.5 32.8999C117.6 32.8999 123.4 38.6999 123.4 45.7999C123.4 52.8999 117.6 58.6999 110.5 58.6999H97.5996V45.7999Z" fill="#2EB67D"/>
												<path d="M91.0988 45.8001C91.0988 52.9001 85.2988 58.7001 78.1988 58.7001C71.0988 58.7001 65.2988 52.9001 65.2988 45.8001V13.5001C65.2988 6.4001 71.0988 0.600098 78.1988 0.600098C85.2988 0.600098 91.0988 6.4001 91.0988 13.5001V45.8001Z" fill="#2EB67D"/>
												<path d="M78.1988 97.6001C85.2988 97.6001 91.0988 103.4 91.0988 110.5C91.0988 117.6 85.2988 123.4 78.1988 123.4C71.0988 123.4 65.2988 117.6 65.2988 110.5V97.6001H78.1988Z" fill="#ECB22E"/>
												<path d="M78.1988 91.1003C71.0988 91.1003 65.2988 85.3003 65.2988 78.2003C65.2988 71.1003 71.0988 65.3003 78.1988 65.3003H110.499C117.599 65.3003 123.399 71.1003 123.399 78.2003C123.399 85.3003 117.599 91.1003 110.499 91.1003H78.1988Z" fill="#ECB22E"/>
											</svg>
										{{end}}
										<div>
											{{if eq $notification.Type "smtp"}}
												{{if $notification.Name}}
													<span>{{$notification.Name}}</span>
												{{else}}
													<span>{{$notification.Details.Host}}</span>
												{{end}}
											{{else}}
												<span>{{$notification.Name}}</span>
											{{end}}
											{{if eq $notification.Type "smtp"}}
											<span>{{$notification.Details.Host}}</span>
											{{end}}
										</div>
									</div>
									{{if not $.Ctx.ConfigFile}}
										<div class="menu">
											<button class="menu-button">
												<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
													<path d="M3 10a1.5 1.5 0 113 0 1.5 1.5 0 01-3 0zM8.5 10a1.5 1.5 0 113 0 1.5 1.5 0 01-3 0zM15.5 8.5a1.5 1.5 0 100 3 1.5 1.5 0 000-3z" />
												</svg>
											</button>

											<dialog>
												<a href="/admin/notifications/{{$notification.ID}}/edit" hx-boost="true">Edit</a>
												<button onclick="document.getElementById('dialog-channel-{{$notification.ID}}').showModal();">Delete</button>
											</dialog>
										</div>
										<dialog class="modal" id="dialog-channel-{{$notification.ID}}">
											<span>Delete {{$notification.Name}}</span>
											<form hx-delete="/admin/notifications/{{$notification.ID}}" hx-swap="none">
												<div>
													<button onclick="document.getElementById('dialog-channel-{{$notification.ID}}').close(); return false;">Cancel</button>
													<button><span></span>Delete</button>
												</div>
											</form>
										</dialog>
									{{end}}
								{{if $.Ctx.ConfigFile}}
								</a>
								{{else}}
								</div>
								{{end}}
							{{end}}
						</div>
					{{end}}
				</div>

				<div class="notification-channels-header">
					<h2>Mail groups</h2>
					{{if not .Ctx.ConfigFile}}
						<div>
							<a href="/admin/notifications/mail-groups/create" hx-boost="true">
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
									<path d="M10.75 4.75a.75.75 0 00-1.5 0v4.5h-4.5a.75.75 0 000 1.5h4.5v4.5a.75.75 0 001.5 0v-4.5h4.5a.75.75 0 000-1.5h-4.5v-4.5z" />
								</svg>
							</a>
						</div>
					{{end}}
				</div>

				{{if eq (len .MailGroups) 0}}
					<div class="entity-empty-state">
						<div class="icon">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path d="M3 4a2 2 0 0 0-2 2v1.161l8.441 4.221a1.25 1.25 0 0 0 1.118 0L19 7.162V6a2 2 0 0 0-2-2H3Z" />
								<path d="m19 8.839-7.77 3.885a2.75 2.75 0 0 1-2.46 0L1 8.839V14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2V8.839Z" />
							</svg>
						</div>
						<span>Create your first mail group</span>
						{{if not .Ctx.ConfigFile}}
							<a class="action" href="/admin/notifications/mail-groups/create" hx-boost="true">Add mail group</a>
						{{else}}
							<a class="action" href="/admin/settings#config-form" hx-boost="true">Go to settings</a>
						{{end}}
					</div>
				{{else}}
					<div class="notifications-list">
						{{range $mailGroup := .MailGroups}}
							{{if $.Ctx.ConfigFile}}
							<a href="/admin/notifications/mail-groups/{{$mailGroup.ID}}/view" hx-boost="true">
							{{else}}
							<div>
							{{end}}
								<div>
									<div>
										<span>{{$mailGroup.Name}}</span>
										{{if $mailGroup.Description}}
											<span>{{$mailGroup.Description}}</span>
										{{end}}
									</div>
								</div>
								{{if not $.Ctx.ConfigFile}}
									<div class="menu">
										<button class="menu-button">
											<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
												<path d="M3 10a1.5 1.5 0 113 0 1.5 1.5 0 01-3 0zM8.5 10a1.5 1.5 0 113 0 1.5 1.5 0 01-3 0zM15.5 8.5a1.5 1.5 0 100 3 1.5 1.5 0 000-3z" />
											</svg>
										</button>

										<dialog>
											<a href="/admin/notifications/mail-groups/{{$mailGroup.ID}}/edit" hx-boost="true">Edit</a>
											<button onclick="document.getElementById('dialog-mail-group-{{$mailGroup.ID}}').showModal();">Delete</button>
										</dialog>
									</div>
									<dialog class="modal" id="dialog-mail-group-{{$mailGroup.ID}}">
										<span>Delete {{$mailGroup.Name}}</span>
										<form hx-delete="/admin/notifications/mail-groups/{{$mailGroup.ID}}" hx-swap="none">
											<div>
												<button onclick="document.getElementById('dialog-mail-group-{{$mailGroup.ID}}').close(); return false;">Cancel</button>
												<button><span></span>Delete</button>
											</div>
										</form>
									</dialog>
								{{end}}
							{{if $.Ctx.ConfigFile}}
							</a>
							{{else}}
							</div>
							{{end}}
						{{end}}
					</div>
				{{end}}
			</div>
		{{end}}
	`

	tmpl, err := parseTmpl("notifications", markup)
	if err != nil {
		log.Printf("notifications.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Notifications []NotificationChannel
			MailGroups    []MailGroup
			Ctx           pageCtx
		}{
			Notifications: channels,
			MailGroups:    mailGroups,
			Ctx:           getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("notifications.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getCreateNotification(w http.ResponseWriter, r *http.Request) {
	const markup = `
		{{define "title"}}Create notification channel{{end}}
		{{define "body"}}
			<div class="create-service-container">
				<div class="admin-nav-header">
					<div>
						<a href="/admin/notifications" hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M11.78 5.22a.75.75 0 0 1 0 1.06L8.06 10l3.72 3.72a.75.75 0 1 1-1.06 1.06l-4.25-4.25a.75.75 0 0 1 0-1.06l4.25-4.25a.75.75 0 0 1 1.06 0Z" clip-rule="evenodd" />
							</svg>
						 </a>
				  
						<h2>Create notification channel</h2>
					</div>
				</div>

				<form hx-post hx-swap="none" autocomplete="off">
					<script>
						function onNotificationTypeSelected(input) {
							if (input.value === "smtp") {
								document.querySelector(".smtp-container").classList.add("smtp-container--visible");
								document.querySelector(".smtp-container").disabled = false;

								document.querySelector(".slack-container").classList.remove("slack-container--visible");
								document.querySelector(".slack-container").disabled = true;
							}

							if (input.value === "slack") {
								document.querySelector(".slack-container").classList.add("slack-container--visible");
								document.querySelector(".slack-container").disabled = false;
										
								document.querySelector(".smtp-container").classList.remove("smtp-container--visible");
								document.querySelector(".smtp-container").disabled = true;
							}
						}
					</script>

					<label>
						Type
					</label>
					<div class="notification-type-group">
						<label>
							<input
								type="radio"
								name="type"
								value="smtp"
								onclick="onNotificationTypeSelected(this);"
								autocomplete="off" 
								checked
								required
							/>
							<span>
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
									<path d="M3 4a2 2 0 00-2 2v1.161l8.441 4.221a1.25 1.25 0 001.118 0L19 7.162V6a2 2 0 00-2-2H3z" />
									<path d="M19 8.839l-7.77 3.885a2.75 2.75 0 01-2.46 0L1 8.839V14a2 2 0 002 2h14a2 2 0 002-2V8.839z" />
								</svg>
								SMTP
							</span>
						</label>
						<label>
							<input 
								type="radio"
								name="type"
								value="slack"
								onclick="onNotificationTypeSelected(this);" 
								autocomplete="off" 
								required
							/>
							<span>
								<svg viewBox="0 0 124 124" fill="none" xmlns="http://www.w3.org/2000/svg">
									<path d="M26.3996 78.2003C26.3996 85.3003 20.5996 91.1003 13.4996 91.1003C6.39961 91.1003 0.599609 85.3003 0.599609 78.2003C0.599609 71.1003 6.39961 65.3003 13.4996 65.3003H26.3996V78.2003Z" fill="#E01E5A"/>
									<path d="M32.9004 78.2003C32.9004 71.1003 38.7004 65.3003 45.8004 65.3003C52.9004 65.3003 58.7004 71.1003 58.7004 78.2003V110.5C58.7004 117.6 52.9004 123.4 45.8004 123.4C38.7004 123.4 32.9004 117.6 32.9004 110.5V78.2003Z" fill="#E01E5A"/>
									<path d="M45.8004 26.4001C38.7004 26.4001 32.9004 20.6001 32.9004 13.5001C32.9004 6.4001 38.7004 0.600098 45.8004 0.600098C52.9004 0.600098 58.7004 6.4001 58.7004 13.5001V26.4001H45.8004Z" fill="#36C5F0"/>
									<path d="M45.7996 32.8999C52.8996 32.8999 58.6996 38.6999 58.6996 45.7999C58.6996 52.8999 52.8996 58.6999 45.7996 58.6999H13.4996C6.39961 58.6999 0.599609 52.8999 0.599609 45.7999C0.599609 38.6999 6.39961 32.8999 13.4996 32.8999H45.7996Z" fill="#36C5F0"/>
									<path d="M97.5996 45.7999C97.5996 38.6999 103.4 32.8999 110.5 32.8999C117.6 32.8999 123.4 38.6999 123.4 45.7999C123.4 52.8999 117.6 58.6999 110.5 58.6999H97.5996V45.7999Z" fill="#2EB67D"/>
									<path d="M91.0988 45.8001C91.0988 52.9001 85.2988 58.7001 78.1988 58.7001C71.0988 58.7001 65.2988 52.9001 65.2988 45.8001V13.5001C65.2988 6.4001 71.0988 0.600098 78.1988 0.600098C85.2988 0.600098 91.0988 6.4001 91.0988 13.5001V45.8001Z" fill="#2EB67D"/>
									<path d="M78.1988 97.6001C85.2988 97.6001 91.0988 103.4 91.0988 110.5C91.0988 117.6 85.2988 123.4 78.1988 123.4C71.0988 123.4 65.2988 117.6 65.2988 110.5V97.6001H78.1988Z" fill="#ECB22E"/>
									<path d="M78.1988 91.1003C71.0988 91.1003 65.2988 85.3003 65.2988 78.2003C65.2988 71.1003 71.0988 65.3003 78.1988 65.3003H110.499C117.599 65.3003 123.399 71.1003 123.399 78.2003C123.399 85.3003 117.599 91.1003 110.499 91.1003H78.1988Z" fill="#ECB22E"/>
								</svg>
								Slack
							</span>
						</label>
					</div>

					<label>
						Display name
						<input name="display-name" required />
					</label>
					
					<fieldset class="smtp-container smtp-container--visible">
						<legend class="hide">SMTP details</legend>
						<label>
							Host
							<input name="host" oninput="onInputHost(this);" required />
						</label>

						<label>
							Port
							<input name="port" type="number" required />
						</label>

						<label>
							Username
							<input name="username" type="password" required />
						</label>

						<label>
							Password
							<input name="password" type="password" required />
						</label>

						<label>
							From
							<input name="from" type="email" required />
						</label>

						<div></div>

						<fieldset id="postmark" class="postmark" style="display: none;" disabled>
							<label>
								Postmark transactional stream
								<input name="pm-transactional" value="outbound" required />
							</label>

							<label>
								Postmark broadcast stream
								<input name="pm-broadcast" value="broadcast" required />
							</label>
						</fieldset>

						<div class="smtp-headers-container">
							<fieldset class="param-box">
								<legend>Headers</legend>
								<div class="entity-empty-state entity-empty-state--secondary">
									<div class="icon">
										<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor">
											<path d="M3 4.75a1 1 0 1 0 0-2 1 1 0 0 0 0 2ZM6.25 3a.75.75 0 0 0 0 1.5h7a.75.75 0 0 0 0-1.5h-7ZM6.25 7.25a.75.75 0 0 0 0 1.5h7a.75.75 0 0 0 0-1.5h-7ZM6.25 11.5a.75.75 0 0 0 0 1.5h7a.75.75 0 0 0 0-1.5h-7ZM4 12.25a1 1 0 1 1-2 0 1 1 0 0 1 2 0ZM3 9a1 1 0 1 0 0-2 1 1 0 0 0 0 2Z" />
										</svg>
									</div>
									<span>No headers set</span>
									<button
										class="action"
										type="button"
										onclick="addParamOnClick(this);"
									>
										Add header
									</button>
								</div>
								<fieldset class="param-box__inputs" disabled>
									<legend class="hide">Request headers list</legend>
									<div class="param-box__list">
										<div class="param-box__item">
											<input name="header-key" required placeholder="Key" />
											<input name="header-value" required placeholder="Value" />
											<button type="button" onclick="removeParamOnClick(this);">
												<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20">
													<path d="M6.28 5.22a.75.75 0 00-1.06 1.06L8.94 10l-3.72 3.72a.75.75 0 101.06 1.06L10 11.06l3.72 3.72a.75.75 0 101.06-1.06L11.06 10l3.72-3.72a.75.75 0 00-1.06-1.06L10 8.94 6.28 5.22z" />
												</svg>
											</button>
										</div>
									</div>
									<button class="param-box__add" type="button" onclick="addParamOnClick(this);">
										<div>
											<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
												<path d="M10.75 4.75a.75.75 0 00-1.5 0v4.5h-4.5a.75.75 0 000 1.5h4.5v4.5a.75.75 0 001.5 0v-4.5h4.5a.75.75 0 000-1.5h-4.5v-4.5z" />
											</svg>
										</div>
										<span>Add header</span>
									</button>
								</fieldset>
							</fieldset>
						</div>
					</fieldset>

					<fieldset class="slack-container" disabled>
						<legend class="hide">Slack info</legend>
						<label>
							Webhook URL
							<input name="webhook-url" type="url" required />

							<button 
								type="button"
								class="help"
								onclick="document.querySelector('.slack-tutorial').classList.toggle('slack-tutorial--visible');"
							>
								How do I get a webhook URL?
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="w-4 h-4">
									<path fill-rule="evenodd" d="M4.22 6.22a.75.75 0 0 1 1.06 0L8 8.94l2.72-2.72a.75.75 0 1 1 1.06 1.06l-3.25 3.25a.75.75 0 0 1-1.06 0L4.22 7.28a.75.75 0 0 1 0-1.06Z" clip-rule="evenodd" />
								</svg>
							</button>
						</label>

						<div class="slack-tutorial">
							<p>You'll need to create a Slack app (if you haven't already) and then add a new webhook to your workspace.</p>

							<a href="https://api.slack.com/apps/new" target="_blank">
								<svg viewBox="0 0 124 124" fill="none" xmlns="http://www.w3.org/2000/svg">
									<path d="M26.3996 78.2003C26.3996 85.3003 20.5996 91.1003 13.4996 91.1003C6.39961 91.1003 0.599609 85.3003 0.599609 78.2003C0.599609 71.1003 6.39961 65.3003 13.4996 65.3003H26.3996V78.2003Z" fill="#E01E5A"/>
									<path d="M32.9004 78.2003C32.9004 71.1003 38.7004 65.3003 45.8004 65.3003C52.9004 65.3003 58.7004 71.1003 58.7004 78.2003V110.5C58.7004 117.6 52.9004 123.4 45.8004 123.4C38.7004 123.4 32.9004 117.6 32.9004 110.5V78.2003Z" fill="#E01E5A"/>
									<path d="M45.8004 26.4001C38.7004 26.4001 32.9004 20.6001 32.9004 13.5001C32.9004 6.4001 38.7004 0.600098 45.8004 0.600098C52.9004 0.600098 58.7004 6.4001 58.7004 13.5001V26.4001H45.8004Z" fill="#36C5F0"/>
									<path d="M45.7996 32.8999C52.8996 32.8999 58.6996 38.6999 58.6996 45.7999C58.6996 52.8999 52.8996 58.6999 45.7996 58.6999H13.4996C6.39961 58.6999 0.599609 52.8999 0.599609 45.7999C0.599609 38.6999 6.39961 32.8999 13.4996 32.8999H45.7996Z" fill="#36C5F0"/>
									<path d="M97.5996 45.7999C97.5996 38.6999 103.4 32.8999 110.5 32.8999C117.6 32.8999 123.4 38.6999 123.4 45.7999C123.4 52.8999 117.6 58.6999 110.5 58.6999H97.5996V45.7999Z" fill="#2EB67D"/>
									<path d="M91.0988 45.8001C91.0988 52.9001 85.2988 58.7001 78.1988 58.7001C71.0988 58.7001 65.2988 52.9001 65.2988 45.8001V13.5001C65.2988 6.4001 71.0988 0.600098 78.1988 0.600098C85.2988 0.600098 91.0988 6.4001 91.0988 13.5001V45.8001Z" fill="#2EB67D"/>
									<path d="M78.1988 97.6001C85.2988 97.6001 91.0988 103.4 91.0988 110.5C91.0988 117.6 85.2988 123.4 78.1988 123.4C71.0988 123.4 65.2988 117.6 65.2988 110.5V97.6001H78.1988Z" fill="#ECB22E"/>
									<path d="M78.1988 91.1003C71.0988 91.1003 65.2988 85.3003 65.2988 78.2003C65.2988 71.1003 71.0988 65.3003 78.1988 65.3003H110.499C117.599 65.3003 123.399 71.1003 123.399 78.2003C123.399 85.3003 117.599 91.1003 110.499 91.1003H78.1988Z" fill="#ECB22E"/>
								</svg>
								Create new Slack app
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="w-4 h-4">
									<path d="M6.22 8.72a.75.75 0 0 0 1.06 1.06l5.22-5.22v1.69a.75.75 0 0 0 1.5 0v-3.5a.75.75 0 0 0-.75-.75h-3.5a.75.75 0 0 0 0 1.5h1.69L6.22 8.72Z" />
									<path d="M3.5 6.75c0-.69.56-1.25 1.25-1.25H7A.75.75 0 0 0 7 4H4.75A2.75 2.75 0 0 0 2 6.75v4.5A2.75 2.75 0 0 0 4.75 14h4.5A2.75 2.75 0 0 0 12 11.25V9a.75.75 0 0 0-1.5 0v2.25c0 .69-.56 1.25-1.25 1.25h-4.5c-.69 0-1.25-.56-1.25-1.25v-4.5Z" />
								</svg>
							</a>

							<p>Select the "From scratch" option</p>
							<img 
								src="/static/images/slack-notification-tutorial/1.png"
								style="width: 60%;"
							/>

							<p>Name your app, choose your Slack workspace, and click “Create app”</p>
							<img 
								src="/static/images/slack-notification-tutorial/2.png"
								style="width: 60%;"
							/>


							<p>Activate incoming webhooks in your app and click “Add New Webhook to Workspace”</p>
							<img 
								src="/static/images/slack-notification-tutorial/3.png"
								style="width: 100%;"
							/>


							<p>Select the Slack channel you'd like to receive notifications in</p>
							<img
								src="/static/images/slack-notification-tutorial/4.png"
								style="width: 60%;"
							/>

							<p>Copy and paste your new webhook URL into statusnook</p>
							<img 
								src="/static/images/slack-notification-tutorial/5.png"
								style="width: 100%;"
							/>
						</div>
					</fieldset>

					<div>
						<button type="submit">Create</button>
					</div>
				</form>
			</div>
			<script>
				function onInputHost(e) {
					const postmarkFieldSet = document.querySelector("#postmark");

					if (e.value.toLowerCase() === "smtp.postmarkapp.com") {
						postmarkFieldSet.style.display = "flex";
						postmarkFieldSet.disabled = false;
					} else {
						postmarkFieldSet.style.display = "none";
						postmarkFieldSet.disabled = true;
					}
				}

				function addParamOnClick(e) {
					const root = e.closest(".param-box");

					const paramBoxInputs = root.querySelector(".param-box__inputs");

					if (paramBoxInputs.disabled) {
						paramBoxInputs.disabled = false;
						root.querySelector(".entity-empty-state").style.display = "none";
						return;
					}

					const items = root.querySelectorAll(".param-box__item")

					const clone = items[0].cloneNode(true);

					const paramBoxList = root.querySelector(".param-box__list")
							
					const insertedClone = paramBoxList.appendChild(
						clone,
					);

					insertedClone.querySelectorAll("input").forEach(v => {
						v.value = "";
					});
				}
				
				function removeParamOnClick(e) {
					const root = e.closest(".param-box");

					const paramBoxInputs = root.querySelector(".param-box__inputs");
					
					const items = paramBoxInputs.querySelectorAll(".param-box__item");
					if (items.length === 1) {								
						const emptyState = root.querySelector(".entity-empty-state");
						emptyState.style.display = "flex";
						root.querySelector(".param-box__inputs").disabled = true;
						[...root.querySelectorAll("input")].forEach(v => v.value = "");
					} else {
						e.parentElement.remove();
					}
				}
			</script>
		{{end}}
	`

	tmpl, err := parseTmpl("getCreateNotification", markup)
	if err != nil {
		log.Printf("getCreateNotification.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Ctx pageCtx
		}{
			Ctx: getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getCreateNotification.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
