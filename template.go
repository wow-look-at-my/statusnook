package main

import (
	"fmt"
	"html/template"
	textTemplate "text/template"
)

var tmpls = map[string]*template.Template{}

func parseTmpl(name string, markup string) (*template.Template, error) {
	if tmpl, ok := tmpls[name]; ok {
		return tmpl, nil
	}

	const rootTmpl = `
		<!DOCTYPE html>
		<html>
			<head>
				<title>{{template "title" .}}</title>
				<link rel="stylesheet" href="/static/main.css">
				<script type="text/javascript" src="/static/htmx-1.9.12.js"></script>
				<meta name="viewport" content="width=device-width, initial-scale=1" />
			</head>
			<body hx-history="false">
				<div class="root">
					<div class="page">
						{{if .Ctx.Status}}
							{{if and (not .Ctx.HideUnconfirmedDomain) (and .Ctx.Auth.ID .Ctx.UnconfirmedDomainProblem)}}
								<div class="banner">
									<span>
										<span class="title">Action required</span>: 
										issues acquiring certificate for '{{.Ctx.UnconfirmedDomain}}'
									</span>
									<a href="/admin/settings" hx-boost="true">
										click here for details
									</a>
								</div>
							{{end}}

							<div class="status-header">
								<div>
									<a id="nook-name" href="/" hx-boost="true">{{.Ctx.Name}}</a>
									{{if .Ctx.AdminArea}}
										<input id="nav-toggle" type="checkbox">
										<label for="nav-toggle">
											<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
												<path fill-rule="evenodd" d="M2 4.75A.75.75 0 0 1 2.75 4h14.5a.75.75 0 0 1 0 1.5H2.75A.75.75 0 0 1 2 4.75ZM2 10a.75.75 0 0 1 .75-.75h14.5a.75.75 0 0 1 0 1.5H2.75A.75.75 0 0 1 2 10Zm0 5.25a.75.75 0 0 1 .75-.75h14.5a.75.75 0 0 1 0 1.5H2.75a.75.75 0 0 1-.75-.75Z" clip-rule="evenodd" />
											</svg>
										</label>
										<div class="nav nav--mobile">
											<a 
												href="/admin/alerts" 
												{{if eq .Ctx.Nav "alerts"}}class="active-nav"{{end}}
												hx-boost="true"
											>
												Alerts
											</a>
											<a 
												href="/admin/monitors" 
												{{if eq .Ctx.Nav "monitors"}}class="active-nav"{{end}}
												hx-boost="true"
											>
												Monitors
											</a>
											<a 
												href="/admin/services" 
												{{if eq .Ctx.Nav "services"}}class="active-nav"{{end}}
												hx-boost="true"
											>
												Services
											</a>										
											<a 
												href="/admin/notifications" 
												{{if eq .Ctx.Nav "notifications"}}class="active-nav"{{end}}
												hx-boost="true"
											>
												Notifications
											</a>

											<a 
												href="/admin/settings"
												{{if eq .Ctx.Nav "settings"}}class="active-nav"{{end}}
												hx-boost="true"
											>
												Settings
											</a>

											<a 
												href="/admin/update" 
												{{if eq .Ctx.Nav "update"}}class="active-nav"{{end}}
												hx-boost="true"
											>
												Update
											</a>

											<a hx-post="/logout">Log out</a>
										</div>
									{{end}}
								</div> 
								{{if .Ctx.Index}}
									<div>
										<div class="get-updates-container">
											{{if or .HasEmailAlertChannel .HasSlackSetup}}
												<button class="get-updates">
													<span>
														<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
															<path d="M3.105 2.288a.75.75 0 0 0-.826.95l1.414 4.926A1.5 1.5 0 0 0 5.135 9.25h6.115a.75.75 0 0 1 0 1.5H5.135a1.5 1.5 0 0 0-1.442 1.086l-1.414 4.926a.75.75 0 0 0 .826.95 28.897 28.897 0 0 0 15.293-7.155.75.75 0 0 0 0-1.114A28.897 28.897 0 0 0 3.105 2.288Z" />
														</svg>
														Get updates
													</span>
													<span></span>
												</button>
											{{end}}
										
											<dialog>
												{{if .HasEmailAlertChannel}}
													<button onclick="document.querySelector('.email-updates-modal').showModal();">
														<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
															<path d="M3 4a2 2 0 0 0-2 2v1.161l8.441 4.221a1.25 1.25 0 0 0 1.118 0L19 7.162V6a2 2 0 0 0-2-2H3Z" />
															<path d="m19 8.839-7.77 3.885a2.75 2.75 0 0 1-2.46 0L1 8.839V14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2V8.839Z" />
														</svg>
														Email
													</button>
												{{end}}
												{{if and .HasEmailAlertChannel .HasSlackSetup}}
													<hr>
												{{end}}
												{{if .HasSlackSetup}}
													<a href="{{.HasSlackSetup}}" target="_blank">
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
													</a>
												{{end}}
											</dialog>
										</div>
										{{if and .Ctx.Index .Ctx.Auth.ID}}
											<a class="icon-button" href="/admin/alerts" hx-boost="true">
												<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
													<path fill-rule="evenodd" d="M2.5 3A1.5 1.5 0 001 4.5v4A1.5 1.5 0 002.5 10h6A1.5 1.5 0 0010 8.5v-4A1.5 1.5 0 008.5 3h-6zm11 2A1.5 1.5 0 0012 6.5v7a1.5 1.5 0 001.5 1.5h4a1.5 1.5 0 001.5-1.5v-7A1.5 1.5 0 0017.5 5h-4zm-10 7A1.5 1.5 0 002 13.5v2A1.5 1.5 0 003.5 17h6a1.5 1.5 0 001.5-1.5v-2A1.5 1.5 0 009.5 12h-6z" clip-rule="evenodd" />
												</svg>
											</a>
										{{end}}
									</div>
								{{else if .Ctx.AdminArea}}
									<div class="nav">
										<a 
											href="/admin/alerts" 
											{{if eq .Ctx.Nav "alerts"}}class="active-nav"{{end}}
											hx-boost="true"
										>
											Alerts
										</a>
										<a 
											href="/admin/monitors" 
											{{if eq .Ctx.Nav "monitors"}}class="active-nav"{{end}}
											hx-boost="true"
										>
											Monitors
										</a>
										<a 
											href="/admin/services" 
											{{if eq .Ctx.Nav "services"}}class="active-nav"{{end}}
											hx-boost="true"
										>
											Services
										</a>										
										<a 
											href="/admin/notifications" 
											{{if eq .Ctx.Nav "notifications"}}class="active-nav"{{end}}
											hx-boost="true"
										>
											Notifications
										</a>

										<div id="nav-menu" class="menu" hx-preserve>
											<button class="menu-button">
												<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="w-4 h-4">
													<path fill-rule="evenodd" d="M4.22 6.22a.75.75 0 0 1 1.06 0L8 8.94l2.72-2.72a.75.75 0 1 1 1.06 1.06l-3.25 3.25a.75.75 0 0 1-1.06 0L4.22 7.28a.75.75 0 0 1 0-1.06Z" clip-rule="evenodd" />
												</svg>                                        
											</button>

											<dialog>
												<a href="/admin/settings" hx-boost="true">Settings</a>
												<a href="/admin/update" hx-boost="true">Update</a>
												<a hx-post="/logout">Log out</a>
											</dialog>
										</div>
									</div>
								{{end}}
							</div>
						{{end}}
						{{template "body" .}}
					</div>
				</div>

				<script>
					document.body.addEventListener("htmx:beforeSwap", function(evt) {
						if (!evt.detail.shouldSwap) {
							evt.detail.shouldSwap = evt.detail.xhr.status === 400;
						}
					});

					document.body.addEventListener("htmx:configRequest", function(evt) {
						evt.detail.headers["csrf-token"] = "{{.Ctx.Auth.CSRFToken}}";
					});


					function onClick(e) {
						if (!e.target.classList.contains("menu-button")) {
							[...document.querySelectorAll("dialog:not(.modal)")].forEach(e => {e.close();});
							document.removeEventListener("click", onClick);
						}
					}

					[...document.querySelectorAll(".menu-button")].forEach(function(e) {
						const menu = e.closest(".menu");
						if (menu.hasAttribute("hx-preserve")) {
							if (menu.dataset.preserve) {
								return;
							}
							menu.dataset.preserve = true;
						}

						e.addEventListener("click", function() {
							const options = menu.querySelector("dialog");
							if (!options.open) {
								[...document.querySelectorAll("dialog:not(.modal)")].forEach(e => {e.close();});
								options.show();
								document.addEventListener("click", onClick);
							} else {
								options.close();
							}
						});
					});

					{{if .Ctx.ShouldAttemptRedirect}}
						(async () => {
							const getResolveResponse = await fetch(
								"https://" + "{{.Ctx.Domain}}/resolve",
								{method: "GET"}
							);

							if (!getResolveResponse.ok || !getResolveResponse.headers.get("X-Statusnook")) {
								return;
							}

							const postResolveResponse = await fetch(
								window.location.origin + "/admin/resolve",
								{
									method: "POST",
									headers: {
										"csrf-token": "{{.Ctx.Auth.CSRFToken}}"
									}
								}
							);

							if (!postResolveResponse.ok) {
								return;
							}

							const token = await postResolveResponse.text();

							const params = new URLSearchParams({
								token, 
								after: window.location.pathname,
							});

							window.location.href = 
								"https://" + "{{.Ctx.Domain}}/cross-auth?" + params.toString();
						})();
					{{end}}
				</script>
			</body>
		</html>
	`

	tmpl, err := template.New(name).Parse(rootTmpl)
	if err != nil {
		return tmpl, err
	}

	tmpl, err = tmpl.Parse(markup)
	if err != nil {
		return tmpl, err
	}

	tmpls[name] = tmpl

	return tmpl, nil
}

var emailTmpls = map[string]*template.Template{}

func parseEmailTmpl(name string, markup string) (*template.Template, error) {
	if tmpl, ok := emailTmpls[name]; ok {
		return tmpl, nil
	}

	tmpl := template.New(name)

	tmpl, err := tmpl.Parse(markup)
	if err != nil {
		return tmpl, fmt.Errorf("parseEmailTmpl.Parse: %w", err)
	}

	emailTmpls[name] = tmpl

	return tmpl, nil
}

var textTmpls = map[string]*textTemplate.Template{}

func parseTextTmpl(name string, markup string) (*textTemplate.Template, error) {
	if tmpl, ok := textTmpls[name]; ok {
		return tmpl, nil
	}

	tmpl := textTemplate.New(name)

	tmpl, err := tmpl.Parse(markup)
	if err != nil {
		return tmpl, fmt.Errorf("parseTextTmpl.Parse: %w", err)
	}

	textTmpls[name] = tmpl

	return tmpl, nil
}
