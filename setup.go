package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/mholt/acmez/acme"
	"golang.org/x/crypto/bcrypt"
)

var metaSetup atomicString

func postSetupStatusnook(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("X-Statusnook-Setup", "true")
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Expose-Headers", "X-Statusnook-Setup")
}

func getSetupDomain(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("getSetupDomain", getSetupDomainMarkup)
	if err != nil {
		log.Printf("getSetupDomain.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	prefillURLText := ""

	prefillURL, err := url.ParseRequestURI("http://" + r.Host)
	if err != nil {
		log.Printf("getSetupDomain.ParseRequestURI: %s", err)
		return
	}

	prefillIP := net.ParseIP(prefillURL.Hostname())
	if prefillIP.String() == "<nil>" {
		prefillURLText = prefillURL.Hostname()
	}

	dev := "false"
	if BUILD == "dev" {
		dev = "true"
	}

	err = tmpl.Execute(
		w,
		struct {
			DEV            template.JS
			SSL            string
			PrefillURLText string
			Ctx            map[string]string
		}{
			DEV:            template.JS(dev),
			SSL:            metaSSL.Load(),
			PrefillURLText: prefillURLText,
			Ctx:            map[string]string{},
		},
	)
	if err != nil {
		log.Printf("getSetupDomain.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postSetupDomain(w http.ResponseWriter, r *http.Request) {
	domainParam := strings.ToLower(r.PostFormValue("domain"))
	if domainParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert domain-alert" hx-swap-oob="true">
				Domain is required
			</div>
		`))
		return
	}

	domainPattern := regexp.MustCompile(`^[a-z0-9]+(?:[\-.][a-z0-9]+)*\.[a-z]+$`)

	if strings.Contains(domainParam, "/") {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert domain-alert" hx-swap-oob="true">
				It looks like you've entered a URL, please enter a domain
			</div>
		`))
		return
	}

	if net.ParseIP(domainParam).String() != "<nil>" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert domain-alert" hx-swap-oob="true">
				It looks like you've entered an IP address, please enter a domain
			</div>
		`))
		return
	}

	if !domainPattern.MatchString(domainParam) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert domain-alert" hx-swap-oob="true">
				Invalid domain
			</div>
		`))
		return
	}

	if BUILD == "release" && metaSSL.Load() == "true" {
		found, err := lookupDomain(domainParam)
		if err != nil {
			log.Printf("postSetupDomain.lookupDomain: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`
				<div id="alert" class="alert domain-alert" hx-swap-oob="true">
					An unhandled error occurred
				</div>
			`))
			return
		}

		if !found {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`
				<div id="alert" class="alert domain-alert" hx-swap-oob="true">
					<span>
						We didn't find your domain's A record, verify it exists and then retry
					</span>

					<span>
						If your domain and A record is correct, you might need to wait a few minutes before retrying
					</span>
				</div>

				<div id="skip-domain-setup" class="skip-domain-setup" hx-swap-oob="true">
					<p>We can also monitor things in the background and redirect you when your domain is ready</p>
					<form onsubmit="onSubmitSkipDomain(this);" hx-post="/setup/skip-domain" hx-swap="none">
						<input name="domain" type="hidden">
						<button>Skip ahead</button>
					</form>

					<script>
						function onSubmitSkipDomain(form) {
							const domain = document.querySelector(".setup-domain").elements.domain.value;
							form.elements.domain.value = domain;
						}
					</script>
				</div>
			`))
			return
		}

		err = certmagic.ManageSync(r.Context(), []string{domainParam})
		if err != nil {
			var acmeProblem acme.Problem
			if errors.As(err, &acmeProblem) {
				if msg, ok := acmeProblemTypeMessages[acmeProblem.Type]; ok {
					w.WriteHeader(http.StatusBadRequest)
					w.Write([]byte(
						fmt.Sprintf(`
							<div id="alert" class="alert domain-alert" hx-swap-oob="true">
								%s
							</div>
						`,
							msg,
						),
					))
					return
				}

				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(
					fmt.Sprintf(`
						<div id="alert" class="alert domain-alert" hx-swap-oob="true">
							An unhandled error occurred %s
						</div>
						`,
						acmeProblem.Type,
					),
				))
				return
			}

			log.Printf("postSetupDomain.ManageSync: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`
				<div id="alert" class="alert domain-alert" hx-swap-oob="true">
					An unexpected error occurred
				</div>
			`))
			return
		}
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSetupDomain.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMetaValue(tx, "domain", domainParam)
	if err != nil {
		log.Printf("postSetupDomain.updateMetaValueName: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMetaValue(tx, "setup", "account")
	if err != nil {
		log.Printf("postSetupDomain.updateMetaValueSetup: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("postSetupDomain.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaSetup.Store("account")
	metaDomain.Store(domainParam)

	if BUILD == "dev" || metaSSL.Load() == "false" {
		w.Header().Add("HX-Location", "/setup/account")
	}
}

func postSetupDomainSkip(w http.ResponseWriter, r *http.Request) {
	domainParam := strings.ToLower(r.PostFormValue("domain"))
	if domainParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	domainPattern := regexp.MustCompile(`^[a-z0-9]+(?:[\-.][a-z0-9]+)*\.[a-z]+$`)

	if !domainPattern.MatchString(domainParam) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSetupDomainSkip.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMetaValue(tx, "unconfirmedDomain", domainParam)
	if err != nil {
		log.Printf("postSetupDomainSkip.updateMetaValueName: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMetaValue(tx, "setup", "account")
	if err != nil {
		log.Printf("postSetupDomainSkip.updateMetaValueSetup: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("postSetupDomainSkip.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaSetup.Store("account")
	metaUnconfirmedDomain.Store(domainParam)

	appWg.Add(1)
	go monitorUnconfirmedDomainLoop(appCtx, &appWg)

	w.Header().Add("HX-Location", "/setup/account")
}

func getSetupAccount(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("getSetupAccount", getSetupAccountMarkup)
	if err != nil {
		log.Printf("getSetup.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, nil)
	if err != nil {
		log.Printf("getSetup.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postSetupAccount(w http.ResponseWriter, r *http.Request) {
	username := r.PostFormValue("username")
	if username == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Username is required
			</div>
		`))
		return
	}

	password := r.PostFormValue("password")
	passwordConfirmation := r.PostFormValue("password-confirmation")

	if password != passwordConfirmation {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Passwords do not match
			</div>
		`))
		return
	}

	if len(password) < 8 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Password must contain at least 8 characters
			</div>
		`))
		return
	}

	pwHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("postSetup.GenerateFromPassword: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSetup.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	userID, err := createUser(tx, username, string(pwHash))
	if err != nil {
		log.Printf("postSetup.createUser: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tokenBytes := make([]byte, 32)
	_, err = rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postSetup.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	csrfTokenBytes := make([]byte, 32)
	_, err = rand.Read(csrfTokenBytes)
	if err != nil {
		log.Printf("postSetup.Read2: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.StdEncoding.EncodeToString(tokenBytes)
	csrfToken := base64.StdEncoding.EncodeToString(csrfTokenBytes)

	err = createSession(tx, token, csrfToken, userID)
	if err != nil {
		log.Printf("postSetup.createSession: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMetaValue(tx, "setup", "name")
	if err != nil {
		log.Printf("postSetup.updateMetaValue: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postSetup.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaSetup.Store("name")

	http.SetCookie(
		w,
		&http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			Expires:  time.Now().UTC().Add(sessionLifetime),
			Secure:   BUILD == "release",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)

	w.Header().Add("HX-Location", "/setup/name")
}

func getSetupName(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("getSetupName", getSetupNameMarkup)
	if err != nil {
		log.Printf("getSetup.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, nil)
	if err != nil {
		log.Printf("getSetup.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postSetupName(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Name is required
			</div>
		`))
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSetupName.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMetaValue(tx, "name", name)
	if err != nil {
		log.Printf("postSetupName.updateMetaValueName: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMetaValue(tx, "setup", "done")
	if err != nil {
		log.Printf("postSetupName.updateMetaValueSetup: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postSetupName.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaName.Store(name)
	metaSetup.Store("done")

	w.Header().Add("HX-Location", "/")
}
