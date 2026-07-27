package main

import (
	"errors"
	"fmt"
	"github.com/caddyserver/certmagic"
	"github.com/mholt/acmez/acme"
	"github.com/miekg/dns"
	"html/template"
	"log"
	mathRand "math/rand"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

func postSetupStatusnook(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("X-Statusnook-Setup", "true")
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Expose-Headers", "X-Statusnook-Setup")
}

func getSetupDomain(w http.ResponseWriter, r *http.Request) {
	tmpl, err := parseTmpl("get_setup_domain.html")
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
			SSL:            metaSSL,
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

func lookupDomain(domain string) (bool, error) {
	rootServers := []string{
		"a.root-servers.net",
		"b.root-servers.net",
		"c.root-servers.net",
		"d.root-servers.net",
		"e.root-servers.net",
		"f.root-servers.net",
		"g.root-servers.net",
		"h.root-servers.net",
		"i.root-servers.net",
		"j.root-servers.net",
		"k.root-servers.net",
		"l.root-servers.net",
		"m.root-servers.net",
	}

	rootNS := rootServers[mathRand.Intn(len(rootServers))]
	c := &dns.Client{}
	m := &dns.Msg{}
	m.SetQuestion(dns.Fqdn(domain), dns.TypeA)
	m.SetEdns0(4096, false)
	r, _, err := c.Exchange(m, rootNS+":53")
	if err != nil {
		return false, fmt.Errorf("lookupDomain.rootNS %s %s: %w", domain, rootNS, err)
	}
	if r.Rcode != dns.RcodeSuccess {
		return false, nil
	}

	authorityNS := r.Ns[mathRand.Intn(len(r.Ns))].(*dns.NS).Ns
	m = &dns.Msg{}
	m.SetQuestion(dns.Fqdn(domain), dns.TypeA)
	m.SetEdns0(4096, false)
	r, _, err = c.Exchange(m, authorityNS+":53")
	if err != nil {
		return false, fmt.Errorf("lookupDomain.authorityNS %s %s: %w", domain, authorityNS, err)
	}
	if r.Rcode != dns.RcodeSuccess {
		return false, nil
	}

	domainNS := r.Ns[mathRand.Intn(len(r.Ns))].(*dns.NS).Ns
	m = &dns.Msg{}
	m.SetQuestion(dns.Fqdn(domain), dns.TypeA)
	m.SetEdns0(4096, false)
	r, _, err = c.Exchange(m, domainNS+":53")
	if err != nil {
		return false, fmt.Errorf("lookupDomain.domainNS %s %s: %w", domain, domainNS, err)
	}
	if r.Rcode != dns.RcodeSuccess {
		return false, nil
	}

	return len(r.Answer) > 0, nil
}

var acmeProblemTypeMessages = map[string]string{
	acme.ProblemTypeDNS:                "Let's Encrypt can't find your domain's DNS record, verify it exists and then retry",
	acme.ProblemTypeConnection:         "Let's Encrypt could not reach your server, ensure your server is publicly accessible on ports 80 and 443, then try again",
	acme.ProblemTypeRejectedIdentifier: "Let's Encrypt will not issue certificates for this domain",
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

	if BUILD == "release" && metaSSL == "true" {
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

	metaSetup = "account"
	metaDomain = domainParam

	if BUILD == "dev" || metaSSL == "false" {
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

	metaSetup = "account"
	metaUnconfirmedDomain = domainParam

	appWg.Add(1)
	go monitorUnconfirmedDomainLoop(appCtx, &appWg)

	w.Header().Add("HX-Location", "/setup/account")
}
