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
	const markup = `
		{{define "title"}}Domain - Statusnook Setup{{end}}
		{{define "body"}}
			<div class="auth-dialog-container">
				{{if eq .SSL "true"}}
					<div class="auth-dialog">
						<div>
							<div>
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
									<path fill-rule="evenodd" d="M10 1a4.5 4.5 0 0 0-4.5 4.5V9H5a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2v-6a2 2 0 0 0-2-2h-.5V5.5A4.5 4.5 0 0 0 10 1Zm3 8V5.5a3 3 0 1 0-6 0V9h6Z" clip-rule="evenodd" />
								</svg>		
							</div>
							<h1>Domain configuration</h1>
						</div>

						<div class="setup-domain-description">
							<p>Enter a domain that has an A record which is set to this machine's IP address</p>
							<p>A certificate will be obtained via Let's Encrypt and automatically configured</p>
						</div>

						<form onsubmit="onDomainSubmit(this);" class="setup-domain" hx-post hx-swap="none">
							<div class="domain-alerts">
								<div id="alert"  class="alert domain-alert"></div>
								<div class="alert alert--info domain-alert"></div>
							</div>
							<label>
								Domain
								<input
									oninput="onDomainChange(this);"
									name="domain"
									placeholder="status.example.com"
									{{if .PrefillURLText}}value="{{.PrefillURLText}}"{{end}}
									required
								>
							</label>

							<button>Confirm</button>
							<a class="auth-dialog-continue" href="/setup/account" hx-boost="true">Continue</a>
							<span class="loader"></span>
						</form>

						<div id="skip-domain-setup" class="skip-domain-setup" hx-swap-oob="true">
					</div>
				{{else}}
					<div class="auth-dialog">
						<div>
							<div>
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="w-4 h-4">
									<path fill-rule="evenodd" d="M4.5 2A2.5 2.5 0 0 0 2 4.5v2.879a2.5 2.5 0 0 0 .732 1.767l4.5 4.5a2.5 2.5 0 0 0 3.536 0l2.878-2.878a2.5 2.5 0 0 0 0-3.536l-4.5-4.5A2.5 2.5 0 0 0 7.38 2H4.5ZM5 6a1 1 0 1 0 0-2 1 1 0 0 0 0 2Z" clip-rule="evenodd" />
								</svg>
							</div>
							<h1>Domain configuration</h1>
						</div>

						<div style="margin-bottom: 6.0rem;">
							<p>
								Enter the domain that users will use to visit your Statusnook
							</p>
							<p>
								This domain appears in places such as email links
							</p>
						</div>

						<form class="setup-domain" hx-post hx-swap="none">
							<div id="alert" class="alert"></div>
							<label>
								Domain
								<input
									name="domain"
									placeholder="status.example.com"
									{{if .PrefillURLText}}value="{{.PrefillURLText}}"{{end}}
									required
								>
							</label>

							<button>Confirm</button>
							<span class="loader"></span>
						</form>
					</div>
				{{end}}
			</div>

			<script>
				function onDomainSubmit(el) {
					const skipDomainSetupEl = document.querySelector(".skip-domain-setup");
					if (skipDomainSetupEl) {
						skipDomainSetupEl.innerHTML = "";
					}
					el.elements.domain.readOnly = true;
				}

				function onDomainChange() {
					const skipDomainSetupEl = document.querySelector(".skip-domain-setup");
					if (skipDomainSetupEl) {
						skipDomainSetupEl.innerHTML = "";
					}
				}

				async function browserReachabilityTest() {
					const alert = document.querySelector("#alert");
					const alert2 = document.querySelector(".alert:nth-of-type(2)");
					const form = document.querySelector("form");

					alert.innerHTML = "";
					alert2.innerHTML = "";

					form.classList.add("htmx-request");

					
					const successMsg = "<span>We've successfully obtained and configured your certificate!</span>";
					const infoMsg = "<span>However, we've detected your browser can't resolve the domain just yet.</span>" +
						"<span>You may continue to use Statusnook via the instances IP address, we'll redirect you to the domain once we detect your browser can successfully resolve it.</span>";

					try {
						if (form.elements.domain.value.startsWith("https://")) {
							form.elements.domain.value = form.elements.domain.value.replace(
								"https://", 
								"",
							);
						}

						if (form.elements.domain.value.startsWith("http://")) {
							form.elements.domain.value = form.elements.domain.value.replace(
								"http://", 
								"",
							);
						}

						const testResponse = await fetch(
							"https://" + form.elements.domain.value + "/setup/statusnook",
							{method: "POST", mode: "cors"}
						);

						if (!testResponse.ok) {
							alert.classList.add("alert--success");
							alert.innerHTML = successMsg;
							alert2.innerHTML = infoMsg;
							[...form.querySelectorAll("label, button")].forEach((v) => {
								v.style.display = "none";
							});
							form.querySelector(".auth-dialog-continue").style.display = "block";
							return;
						}

						if (!testResponse.headers.get("X-Statusnook-Setup")) {
							alert.classList.add("alert--success");
							alert.innerHTML = successMsg;
							alert2.innerHTML = infoMsg;
							[...form.querySelectorAll("label, button")].forEach((v) => {
								v.style.display = "none";
							});
							form.querySelector(".auth-dialog-continue").style.display = "block";
							return;
						}

						return true;
					} catch(e) {
						if (Object.keys(e).length === 0) {
							alert.classList.add("alert--success");
							alert.innerHTML = successMsg;
							alert2.innerHTML = infoMsg;
							[...form.querySelectorAll("label, button")].forEach((v) => {
								v.style.display = "none";
							});
							form.querySelector(".auth-dialog-continue").style.display = "block";
						}
						return;
					} finally {
						form.classList.remove("htmx-request");
					}
				}

				document.addEventListener('htmx:afterRequest', async function(evt) {
					const form = document.querySelector("form");
					const dev = {{.DEV}};

					form.elements.domain.readOnly = false;
					
					if (evt.detail.pathInfo.responsePath === "/setup/domain" && evt.detail.successful) {
						if (dev || await browserReachabilityTest()) {
							window.location.href = window.location.protocol + "//" + 
								form.elements.domain.value + "/setup/account";
						}
					}
				});
			</script>
		{{end}}
	`

	tmpl, err := parseTmpl("getSetupDomain", markup)
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
