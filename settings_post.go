package main

import (
	"errors"
	"fmt"
	"github.com/mholt/acmez/acme"
	"html"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
)

func postSettings(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	domain := strings.ToLower(r.PostFormValue("domain"))

	// A setting the environment owns is re-applied on every start, so accepting
	// a change here would silently revert on the next restart.
	if name != "" && env.Name != "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write(alertOOB("The name is set by STATUSNOOK_NAME and cannot be changed here"))
		return
	}

	if domain != "" && env.Domain != "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write(alertOOB("The domain is set by STATUSNOOK_DOMAIN and cannot be changed here"))
		return
	}

	if name != "" {
		if metaConfigFileEnabled {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		tx, err := rwDB.Begin()
		if err != nil {
			log.Printf("postSettings.BeginName: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		err = updateMetaValue(tx, "name", name)
		if err != nil {
			log.Printf("postSettings.updateMetaValueName: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if err := tx.Commit(); err != nil {
			log.Printf("postSettings.CommitName: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		metaName = name

		escapedName := html.EscapeString(metaName)

		w.Write([]byte(
			fmt.Sprintf(`
				<input id="name" name="name" value="%s" hx-swap-oob="true" disabled>
				<a id="nook-name" href="/" hx-boost="true" hx-swap-oob="true">%s</a>
			`,
				escapedName,
				escapedName,
			),
		))

		return
	}

	if domain != "" {
		if metaSSL != "true" {
			tx, err := rwDB.Begin()
			if err != nil {
				log.Printf("postSettings.BeginUnmanagedDomain: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(
					`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
				))
				return
			}
			defer tx.Rollback()

			err = updateMetaValue(tx, "domain", domain)
			if err != nil {
				log.Printf("postSettings.updateMetaValueDomainUnmanaged: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(
					`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
				))
				return
			}

			err = tx.Commit()
			if err != nil {
				log.Printf("postSettings.CommitUnmanagedDomain: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(
					`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
				))
				return
			}

			metaDomain = domain

			w.Header().Add("HX-Location", "/admin/settings")
			return
		}

		if metaDomain == "" {
			tx, err := rwDB.Begin()
			if err != nil {
				log.Printf("postSettings.BeginUnconfirmedDomainUpdate: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(
					`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
				))
				return
			}
			defer tx.Rollback()

			err = updateMetaValue(tx, "unconfirmedDomain", domain)
			if err != nil {
				log.Printf("postSettings.updateMetaValueUnconfirmedDomainUpdate: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(
					`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
				))
				return
			}

			if err := tx.Commit(); err != nil {
				log.Printf("postSettings.CommitUnconfirmedDomainUpdate: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(
					`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
				))
				return
			}

			metaUnconfirmedDomain = domain
		}

		domainPattern := regexp.MustCompile(`^[a-z0-9]+(?:[\-.][a-z0-9]+)*\.[a-z]+$`)

		if strings.Contains(domain, "/") {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`
				<div id="banner" class="banner" hx-swap-oob="true">
					It looks like you've entered a URL, please enter a domain
				</div>
			`))
			return
		}

		if net.ParseIP(domain).String() != "<nil>" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`
				<div id="banner" class="banner" hx-swap-oob="true">
					It looks like you've entered an IP address, please enter a domain
				</div>
			`))
			return
		}

		if !domainPattern.MatchString(domain) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`
				<div id="banner" class="banner" hx-swap-oob="true">
					Invalid domain
				</div>
			`))
			return
		}

		found, err := lookupDomain(domain)
		if err != nil {
			log.Printf("postSettings.lookupDomain: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
			))
			return
		}

		if !found {
			notFoundMsg := "We didn't find your domain's A record, verify it exists and then retry. " +
				"If your domain and A record is correct, you might need to wait a few minutes before retrying."

			if metaDomain == "" {
				tx, err := rwDB.Begin()
				if err != nil {
					log.Printf("postSettings.BeginUnconfirmedDomainProblemNotFound: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(
						`
						<div id="banner" class="banner" hx-swap-oob="true">
							<span>An unexpected error occurred</span>
						</div>
					`,
					))
					return
				}
				defer tx.Rollback()

				err = updateMetaValue(tx, "unconfirmedDomainProblem", notFoundMsg)
				if err != nil {
					log.Printf("postSettings.updateMetaValueDomainProblemNotFound: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(
						`
						<div id="banner" class="banner" hx-swap-oob="true">
							<span>An unexpected error occurred</span>
						</div>
					`,
					))
					return
				}

				if err := tx.Commit(); err != nil {
					log.Printf("postSettings.CommitUnconfirmedDomainProblemNotFound: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(
						`
						<div id="banner" class="banner" hx-swap-oob="true">
							span>An unexpected error occurred</span>
						</div>
					`,
					))
					return
				}

				metaUnconfirmedDomainProblem = notFoundMsg
			}

			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(
				fmt.Sprintf(`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>%s</span>
					</div>
				`,
					notFoundMsg,
				),
			))
			return
		}

		err = attemptCertificateAcquisition(r.Context(), domain)
		if err != nil {
			errMsg := "An unexpected error occurred"

			var acmeProblem acme.Problem
			if errors.As(err, &acmeProblem) {
				var ok bool
				errMsg, ok = acmeProblemTypeMessages[acmeProblem.Type]
				if !ok {
					errMsg = "An unhandled error occurred " +
						acmeProblem.Type
				}
			} else {
				log.Printf("postSettings.attemptCertificateAcquisition: %s", err)
			}

			if metaDomain == "" {
				tx, err := rwDB.Begin()
				if err != nil {
					log.Printf("postSettings.BeginUnconfirmedDomainProblem: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(
						`
						<div id="banner" class="banner" hx-swap-oob="true">
							<span>An unexpected error occurred</span>
						</div>
					`,
					))
					return
				}
				defer tx.Rollback()

				err = updateMetaValue(tx, "unconfirmedDomainProblem", errMsg)
				if err != nil {
					log.Printf("postSettings.updateMetaValueDomainProblem: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(
						`
						<div id="banner" class="banner" hx-swap-oob="true">
							<span>An unhandled error occurred</span>
						</div>
					`,
					))
					return
				}

				if err := tx.Commit(); err != nil {
					log.Printf("postSettings.CommitUnconfirmedDomainProblem %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(
						`
						<div id="banner" class="banner" hx-swap-oob="true">
							<span>An unhandled error occurred</span>
						</div>
					`,
					))
					return
				}

				metaUnconfirmedDomainProblem = errMsg
			}

			if errMsg == "An unexpected error occurred" {
				w.WriteHeader(http.StatusInternalServerError)
			} else {
				w.WriteHeader(http.StatusBadRequest)
			}

			w.Write([]byte(
				fmt.Sprintf(`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>%s</span>
					</div>
				`,
					errMsg,
				),
			))
			return
		}

		tx, err := rwDB.Begin()
		if err != nil {
			log.Printf("postSettings.BeginDomain: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
			))
			return
		}
		defer tx.Rollback()

		err = updateMetaValue(tx, "domain", domain)
		if err != nil {
			log.Printf("postSettings.updateMetaValueDomain: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
			))
			return
		}

		err = updateMetaValue(tx, "unconfirmedDomain", "")
		if err != nil {
			log.Printf("postSettings.updateMetaValueUnconfirmedDomain: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
			))
			return
		}

		err = updateMetaValue(tx, "unconfirmedDomainProblem", "")
		if err != nil {
			log.Printf("postSettings.updateMetaValueUnconfirmedDomainProblem: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
			))
			return
		}

		if err := tx.Commit(); err != nil {
			log.Printf("postSettings.CommitDomain: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unhandled error occurred</span>
					</div>
				`,
			))
			return
		}

		metaDomain = domain
		metaUnconfirmedDomain = ""
		metaUnconfirmedDomainProblem = ""

		w.Header().Add("HX-Location", "/admin/settings")
	}
}

func postSettingsCancelDomain(w http.ResponseWriter, r *http.Request) {
	v := "You cancelled the domain verification process. Please enter a new domain."

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSettingsCancelDomain.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMetaValue(tx, "unconfirmedDomainProblem", v)
	if err != nil {
		log.Printf("postSettingsCancelDomain.updateMetaValueProblem: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postSettingsCancelDomain.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaUnconfirmedDomainProblem = v

	w.Header().Add("HX-Location", "/admin/settings")
}
