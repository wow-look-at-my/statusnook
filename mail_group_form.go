package main

import (
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"strconv"
	"strings"
)

func getCreateMailGroup(w http.ResponseWriter, r *http.Request) {
	const markup = `
		{{define "title"}}Create mail group{{end}}
		{{define "body"}}
			<div class="create-service-container">
				<div class="admin-nav-header">
					<div>
						<a href="/admin/notifications" hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M11.78 5.22a.75.75 0 0 1 0 1.06L8.06 10l3.72 3.72a.75.75 0 1 1-1.06 1.06l-4.25-4.25a.75.75 0 0 1 0-1.06l4.25-4.25a.75.75 0 0 1 1.06 0Z" clip-rule="evenodd" />
							</svg>
						</a>
				
						<h2>Create mail group</h2>
					</div>
				</div>

				<form hx-post hx-swap="none">
					<label>
						Name
						<input name="name" required />
					</label>

					<label>
						Description
						<input name="description" />
					</label>
					
					<fieldset class="param-box">
						<legend>Members</legend>
						<div class="entity-empty-state entity-empty-state--secondary">
							<div class="icon">
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor">
									<path d="M3 4.75a1 1 0 1 0 0-2 1 1 0 0 0 0 2ZM6.25 3a.75.75 0 0 0 0 1.5h7a.75.75 0 0 0 0-1.5h-7ZM6.25 7.25a.75.75 0 0 0 0 1.5h7a.75.75 0 0 0 0-1.5h-7ZM6.25 11.5a.75.75 0 0 0 0 1.5h7a.75.75 0 0 0 0-1.5h-7ZM4 12.25a1 1 0 1 1-2 0 1 1 0 0 1 2 0ZM3 9a1 1 0 1 0 0-2 1 1 0 0 0 0 2Z" />
								</svg>
							</div>
							<span>No members</span>
							<button
								class="action"
								type="button"
								onclick="addParamOnClick(this);"
							>
								Add member
							</button>
						</div>
						<fieldset class="param-box__inputs" disabled>
							<legend class="hide">Request headers list</legend>
							<div class="param-box__list">
								<div class="param-box__item">
									<input name="members" type="email" required placeholder="Email address" />
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
								<span>Add member</span>
							</button>
						</fieldset>
					</fieldset>

					<div>
						<button type="submit">Create</button>
					</div>
				</form>
			</div>
			<script>
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

	tmpl, err := parseTmpl("getCreateMailGroup", markup)
	if err != nil {
		log.Printf("getCreateMailGroup.parseTmpl: %s", err)
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
		log.Printf("getCreateMailGroup.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getEditMailGroup(w http.ResponseWriter, r *http.Request) {
	readOnly := strings.HasSuffix(r.URL.Path, "view")
	if !readOnly && metaConfigFileEnabled {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditMailGroup.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	mailGroup, err := getMailGroupByID(tx, id)
	if err != nil {
		log.Printf("getEditMailGroup.getMailGroupByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	mailGroupMembers, err := listMailGroupMembersByID(tx, id)
	if err != nil {
		log.Printf("getEditMailGroup.listMailGroupMembersByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getEditMailGroup.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	const markup = `
		{{define "title"}}
			{{if .ReadOnly}}
				View mail group
			{{else}}
				Edit mail group
			{{end}}
		{{end}}
		{{define "body"}}
			<div class="create-service-container">
				<div class="admin-nav-header">
					<div>
						<a href="/admin/notifications" hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M11.78 5.22a.75.75 0 0 1 0 1.06L8.06 10l3.72 3.72a.75.75 0 1 1-1.06 1.06l-4.25-4.25a.75.75 0 0 1 0-1.06l4.25-4.25a.75.75 0 0 1 1.06 0Z" clip-rule="evenodd" />
							</svg>
						</a>
					
						{{if .ReadOnly}}
							<h2>View mail group</h2>
						{{else}}
							<h2>Edit mail group</h2>
						{{end}}
					</div>
				</div>

				<form hx-post hx-swap="none">
					<label>
						Name
						<input name="name" value="{{.MailGroup.Name}}" required />
					</label>

					<label>
						Description
						<input name="description" value="{{.MailGroup.Description}}" />
					</label>
					
					<fieldset class="param-box">
						<legend>Members</legend>
						<div class="entity-empty-state {{if not .Ctx.ConfigFile}}entity-empty-state--secondary{{end}}"
							{{if .MailGroupMembers}}style="display: none;"{{end}}
						>
							<div class="icon">
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor">
									<path d="M3 4.75a1 1 0 1 0 0-2 1 1 0 0 0 0 2ZM6.25 3a.75.75 0 0 0 0 1.5h7a.75.75 0 0 0 0-1.5h-7ZM6.25 7.25a.75.75 0 0 0 0 1.5h7a.75.75 0 0 0 0-1.5h-7ZM6.25 11.5a.75.75 0 0 0 0 1.5h7a.75.75 0 0 0 0-1.5h-7ZM4 12.25a1 1 0 1 1-2 0 1 1 0 0 1 2 0ZM3 9a1 1 0 1 0 0-2 1 1 0 0 0 0 2Z" />
								</svg>
							</div>
							<span>No members</span>
							{{if not .Ctx.ConfigFile}}
								<button
									class="action"
									type="button"
									onclick="addParamOnClick(this);"
								>
									Add member
								</button>
							{{else}}
								<a class="action" href="/admin/settings#config-form" hx-swap="outerHTML" hx-boost="true">Go to settings</a>
							{{end}}
						</div>
						<fieldset class="param-box__inputs" {{if not .MailGroupMembers}}disabled{{end}}>
							<legend class="hide">Request headers list</legend>
							<div class="param-box__list">
								{{if .MailGroupMembers}}
									{{range $member := .MailGroupMembers}}
										<div class="param-box__item">
											<input 
												name="members"
												type="email"
												required
												placeholder="Email address"
												value="{{$member.EmailAddress}}"
											/>
											<button type="button" onclick="removeParamOnClick(this);">
												<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20">
													<path d="M6.28 5.22a.75.75 0 00-1.06 1.06L8.94 10l-3.72 3.72a.75.75 0 101.06 1.06L10 11.06l3.72 3.72a.75.75 0 101.06-1.06L11.06 10l3.72-3.72a.75.75 0 00-1.06-1.06L10 8.94 6.28 5.22z" />
												</svg>
											</button>
										</div>
									{{end}}
								{{else}}
									<div class="param-box__item">
										<input name="members" type="email" required placeholder="Email address" />
										<button type="button" onclick="removeParamOnClick(this);">
											<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20">
												<path d="M6.28 5.22a.75.75 0 00-1.06 1.06L8.94 10l-3.72 3.72a.75.75 0 101.06 1.06L10 11.06l3.72 3.72a.75.75 0 101.06-1.06L11.06 10l3.72-3.72a.75.75 0 00-1.06-1.06L10 8.94 6.28 5.22z" />
											</svg>
										</button>
									</div>
								{{end}}
							</div>
							<button class="param-box__add" type="button" onclick="addParamOnClick(this);">
								<div>
									<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
										<path d="M10.75 4.75a.75.75 0 00-1.5 0v4.5h-4.5a.75.75 0 000 1.5h4.5v4.5a.75.75 0 001.5 0v-4.5h4.5a.75.75 0 000-1.5h-4.5v-4.5z" />
									</svg>
								</div>
								<span>Add member</span>
							</button>
						</fieldset>
					</fieldset>

					<div>
						{{if not .Ctx.ConfigFile}}
							<button type="submit">Create</button>
						{{end}}
					</div>
				</form>
			</div>
			<script>
				{{if .ReadOnly}}
					[...document.querySelector("form").elements].forEach((v) => {
						if (v.tagName === "FIELDSET") {
							return;
						}
						v.disabled = true;
					});
				{{end}}

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

	tmpl, err := parseTmpl("getEditMailGroup", markup)
	if err != nil {
		log.Printf("getEditMailGroup.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			MailGroup        MailGroup
			MailGroupMembers []MailGroupMember
			ReadOnly         bool
			Ctx              pageCtx
		}{
			MailGroup:        mailGroup,
			MailGroupMembers: mailGroupMembers,
			ReadOnly:         readOnly,
			Ctx:              getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getEditMailGroup.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getViewMailGroup(w http.ResponseWriter, r *http.Request) {
	getEditMailGroup(w, r)
}
