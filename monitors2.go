package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mholt/acmez/acme"
)

func monitorLoop(ctx context.Context, wg *sync.WaitGroup) {
	checkoutMu := sync.RWMutex{}
	lastCheckedMu := sync.RWMutex{}

	lastChecked := map[int]time.Time{}
	checkout := map[int]time.Time{}

	// NewTicker, not Tick: time.Tick leaks its ticker and its goroutine, and
	// these loops do exit -- on ctx.Done, and monitorUnconfirmedDomainLoop
	// returns on its own once the domain resolves.
	ticker := time.NewTicker(time.Millisecond * 500)
	defer ticker.Stop()
	tick := ticker.C

	for {
		select {
		case <-tick:
			func() {
				tx, err := db.Begin()
				if err != nil {
					log.Printf("monitorLoop.BeginListMonitors: %s", err)
					return
				}
				defer tx.Rollback()

				monitors, err := listMonitors(tx)
				if err != nil {
					log.Printf("monitorLoop.listMonitors: %s", err)
					return
				}

				err = tx.Commit()
				if err != nil {
					log.Printf("monitorLoop.CommitListMonitors: %s", err)
					return
				}

				for _, monitor := range monitors {
					monitor := monitor

					httpClient := http.Client{
						Timeout: time.Duration(monitor.Timeout) * time.Second,
					}

					lastCheckedMu.RLock()
					if time.Since(lastChecked[monitor.ID]) <
						time.Second*time.Duration(monitor.Frequency) {
						lastCheckedMu.RUnlock()
						continue
					}
					lastCheckedMu.RUnlock()

					checkoutMu.RLock()
					if _, ok := checkout[monitor.ID]; ok {
						checkoutMu.RUnlock()
						continue
					}
					checkoutMu.RUnlock()

					checkoutMu.Lock()
					checkout[monitor.ID] = time.Now().UTC()
					checkoutMu.Unlock()

					// Registered with the app WaitGroup: these outlive the tick
					// that spawned them by up to attempts x timeout, and on SIGTERM
					// they were racing db.Close() to write their monitor log.
					wg.Add(1)
					go func() {
						defer wg.Done()

						var endedAt time.Time

						defer func() {
							checkoutMu.Lock()
							delete(checkout, monitor.ID)
							checkoutMu.Unlock()

							if endedAt.IsZero() {
								endedAt = time.Now().UTC()
							}

							lastCheckedMu.Lock()
							lastChecked[monitor.ID] = endedAt
							lastCheckedMu.Unlock()
						}()

						startedAt := time.Now().UTC()

						var errorMessage sql.NullString

						var resp *http.Response
						var reqErr error
						var statusCode sql.NullInt64
						result := ""

						attempt := 0
						for attempt = 0; attempt < monitor.Attempts; attempt++ {
							var body io.Reader
							if monitor.Body.Valid {
								body = strings.NewReader(monitor.Body.String)
							}

							// Carries the app context so a check in flight at SIGTERM
							// aborts instead of holding shutdown for up to
							// attempts x timeout.
							monitorReq, err := http.NewRequestWithContext(
								ctx,
								monitor.Method,
								monitor.URL,
								body,
							)
							if err != nil {
								log.Printf("monitorLoop.NewRequest: %s", err)
								break
							}
							for k, v := range monitor.RequestHeaders {
								monitorReq.Header.Add(k, v)
							}

							// Cleared each attempt: a 500 on attempt 1 followed by a
							// connection failure on attempt 2 used to log the failure
							// with attempt 1's status code still attached.
							statusCode = sql.NullInt64{}

							resp, reqErr = httpClient.Do(monitorReq)
							if reqErr != nil {
								continue
							}

							_, err = io.Copy(io.Discard, resp.Body)
							resp.Body.Close()
							if err != nil {
								log.Printf("monitorLoop.Copy: %s", err)
								break
							}

							statusCode = sql.NullInt64{
								Int64: int64(resp.StatusCode),
								Valid: true,
							}

							if resp.StatusCode >= 400 {
								result = "error"
								continue
							}

							result = "success"

							break
						}

						if result != "success" {
							urlErr := &url.Error{}
							if ok := errors.As(reqErr, &urlErr); ok {
								errorMessage = sql.NullString{
									String: reqErr.Error(),
									Valid:  true,
								}

								// Only a genuine timeout is reported as one. A DNS
								// failure, a refused connection and a TLS error are
								// all *url.Error too, and calling every one of them
								// "timeout" sends whoever is debugging the outage
								// looking in the wrong place.
								if urlErr.Timeout() || errors.Is(reqErr, context.DeadlineExceeded) {
									result = "timeout"
								} else {
									result = "error"
								}
							}
						}

						// attempt is the loop counter, which stops at the index of
						// the attempt that succeeded -- so a first-try success was
						// recorded as 0 attempts while a total failure recorded the
						// full count. The column is shown to the operator.
						attemptsMade := attempt
						if result == "success" {
							attemptsMade = attempt + 1
						}

						endedAt = time.Now().UTC()

						tx, err := rwDB.Begin()
						if err != nil {
							log.Printf("monitorLoop.BeginMonitorLog: %s", err)
							return
						}
						defer tx.Rollback()

						monitorLogID, err := createMonitorLog(
							tx,
							startedAt,
							endedAt,
							statusCode.Int64,
							errorMessage,
							attemptsMade,
							result,
							monitor.ID,
						)
						if err != nil {
							log.Printf("monitorLoop.createMonitorLog: %s", err)
							return
						}

						lastChecked, err := getMonitorLogLastChecked(tx, monitor.ID)
						if err != nil {
							log.Printf(
								"monitorLoop.checkNotificationDueByMonitorID: %s",
								err,
							)
							return
						}

						err = createMonitorLogLastChecked(tx, endedAt, monitor.ID, monitorLogID)
						if err != nil {
							log.Printf("monitorLoop.createMonitorLogLastChecked: %s", err)
							return
						}

						err = tx.Commit()
						if err != nil {
							log.Printf("monitorLoop.CommitMonitorLog: %s", err)
							return
						}

						tx, err = db.Begin()
						if err != nil {
							log.Printf("monitorLoop.BeginListNotificationsByMonitorID: %s", err)
							return
						}
						defer tx.Rollback()

						channels, err := listNotificationChannelsByMonitorID(tx, monitor.ID)
						if err != nil {
							log.Printf("monitorLoop.listNotificationChannelsByMonitorID: %s", err)
							return
						}

						err = tx.Commit()
						if err != nil {
							log.Printf("monitorLoop.CommitListNotificationChannelsByMonitorID: %s", err)
							return
						}

						if len(channels) > 0 {
							// ID is 0 only when there is no previous check at all
							// (getMonitorLogLastChecked returns a zero struct on
							// ErrNoRows). Without this a monitor added for an endpoint
							// that is ALREADY down has no happy state to transition
							// from, so it never alerts -- and every later failure looks
							// like more of the same. The team first hears about the
							// outage when it recovers.
							firstCheck := lastChecked.ID == 0

							lastHappy := lastChecked.ResponseCode.Int32 != 0 &&
								lastChecked.ResponseCode.Int32 < 400

							if firstCheck && result != "success" ||
								lastHappy && result != "success" ||
								!lastHappy && result == "success" {
								status := "down"
								if result == "success" {
									status = "up"
								}

								tx, err = db.Begin()
								if err != nil {
									log.Printf("monitorLoop.BeginListMailGroupMembersEmailsByMonitorID: %s", err)
									return
								}
								defer tx.Rollback()

								emailAddresses, err := listMailGroupMembersEmailsByMonitorID(
									tx,
									monitor.ID,
								)
								if err != nil {
									log.Printf("monitorLoop.listMailGroupMembersEmailsByMonitorID: %s", err)
									return
								}

								err = tx.Commit()
								if err != nil {
									log.Printf("monitorLoop.CommitListMailGroupMembersEmailsByMonitorID: %s", err)
									tx.Rollback()
									return
								}

								skipEmail := len(emailAddresses) == 0

								for _, channel := range channels {
									if channel.Type == "smtp" && !skipEmail {
										err := sendMonitorAlertEmail(
											monitor,
											channel,
											statusCode,
											result,
											startedAt,
											emailAddresses,
											status,
										)
										if err != nil {
											log.Printf("monitorLoop.sendMonitorAlertEmail: %s", err)
											continue
										}
										skipEmail = true
									} else if channel.Type == "slack" {
										err = sendMonitorAlertSlack(
											monitor,
											channel,
											statusCode,
											startedAt,
											result,
											httpClient,
											status,
											metaDomain.Load(),
										)
										if err != nil {
											log.Printf("monitorLoop.sendMonitorAlertSlack: %s", err)
											continue
										}
									}
								}
							}
						}
					}()
				}
			}()
		case <-ctx.Done():
			wg.Done()
			return
		}
	}
}

func monitorUnconfirmedDomainLoop(ctx context.Context, wg *sync.WaitGroup) {
	ticker := time.NewTicker(time.Minute * 1)
	defer ticker.Stop()
	tick := ticker.C

	for {
		if metaUnconfirmedDomain.Load() == "" || metaUnconfirmedDomainProblem.Load() != "" {
			wg.Done()
			return
		}

		select {
		case <-tick:
			func() {
				found, err := lookupDomain(metaUnconfirmedDomain.Load())
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.lookupDomain: %s", err)
					return
				}

				if !found {
					return
				}

				err = attemptCertificateAcquisition(ctx, metaUnconfirmedDomain.Load())
				if err != nil {
					unconfirmedDomainProblemMsg := "An unexpected error occurred"

					var acmeProblem acme.Problem
					if errors.As(err, &acmeProblem) {
						var ok bool
						unconfirmedDomainProblemMsg, ok = acmeProblemTypeMessages[acmeProblem.Type]
						if !ok {
							unconfirmedDomainProblemMsg = "An unhandled error occurred " +
								acmeProblem.Type
						}
					} else {
						log.Printf("monitorUnconfirmedDomainLoop.attemptCertificateAcquisition: %s", err)
					}

					tx, err := rwDB.Begin()
					if err != nil {
						log.Printf("monitorUnconfirmedDomainLoop.BeginUnconfirmedDomainProblem: %s", err)
						return
					}
					defer tx.Rollback()

					metaUnconfirmedDomainProblem.Store(unconfirmedDomainProblemMsg)
					err = updateMetaValue(tx, "unconfirmedDomainProblem", metaUnconfirmedDomainProblem.Load())
					if err != nil {
						log.Printf("monitorUnconfirmedDomainLoop.UpdateUnconfirmedDomainProblem: %s", err)
						return
					}

					if err := tx.Commit(); err != nil {
						log.Printf("monitorUnconfirmedDomainLoop.CommitUnconfirmedDomainProblem: %s", err)
						return
					}

					return
				}

				tx, err := rwDB.Begin()
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.Begin: %s", err)
					return
				}
				defer tx.Rollback()

				metaDomain.Store(metaUnconfirmedDomain.Load())
				err = updateMetaValue(tx, "domain", metaUnconfirmedDomain.Load())
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.updateMetaValueDomain: %s", err)
					return
				}

				metaUnconfirmedDomain.Store("")
				err = updateMetaValue(tx, "unconfirmedDomain", "")
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.updateMetaValueUnconfirmedDomain: %s", err)
					return
				}

				metaUnconfirmedDomainProblem.Store("")
				err = updateMetaValue(tx, "unconfirmedDomainProblem", "")
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.updateMetaValueUnconfirmedDomainProblem: %s", err)
					return
				}

				if err := tx.Commit(); err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.Commit: %s", err)
					return
				}
			}()
		case <-ctx.Done():
			wg.Done()
			return
		}
	}
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

type Monitor struct {
	ID             int
	Slug           string
	Name           string
	URL            string
	Method         string
	Frequency      int
	Timeout        int
	Attempts       int
	RequestHeaders map[string]string
	BodyFormat     sql.NullString
	Body           sql.NullString
}

func listMonitors(tx *sql.Tx) ([]Monitor, error) {
	const query = `
		select id, slug, name, url, method, frequency, timeout, attempts, request_headers, 
			body_format, body
		from monitor
	`

	monitorListings := []Monitor{}

	rows, err := tx.Query(query)
	if err != nil {
		return monitorListings, fmt.Errorf("listMonitors.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var serializedRequestHeaders sql.NullString

		monitor := Monitor{}
		err = rows.Scan(
			&monitor.ID,
			&monitor.Slug,
			&monitor.Name,
			&monitor.URL,
			&monitor.Method,
			&monitor.Frequency,
			&monitor.Timeout,
			&monitor.Attempts,
			&serializedRequestHeaders,
			&monitor.BodyFormat,
			&monitor.Body,
		)
		if err != nil {
			return monitorListings, fmt.Errorf("listMonitors.Scan: %w", err)
		}

		requestHeaders := map[string]string{}
		if serializedRequestHeaders.Valid {
			err = json.Unmarshal([]byte(serializedRequestHeaders.String), &requestHeaders)
			if err != nil {
				return monitorListings, fmt.Errorf("listMonitors.Unmarshal: %w", err)
			}
		}

		monitor.RequestHeaders = requestHeaders
		monitorListings = append(monitorListings, monitor)
	}

	if err := rows.Err(); err != nil {
		return monitorListings, fmt.Errorf("listMonitors.RowsErr: %w", err)
	}

	return monitorListings, nil
}

func monitors(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("monitors.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	monitors, err := listMonitors(tx)
	if err != nil {
		log.Printf("monitors.listMonitors: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	lastCheckedLogs, err := listAllMonitorLogLastChecked(tx)
	if err != nil {
		log.Printf("monitors.listAllMonitorLogLastChecked: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	monitorHappy := make(map[int]bool, len(lastCheckedLogs))
	for _, v := range lastCheckedLogs {
		monitorHappy[v.ID] = v.ResponseCode.Int32 != 0 && v.ResponseCode.Int32 < 400
	}

	tmpl, err := parseTmpl("monitors", monitorsMarkup)
	if err != nil {
		log.Printf("monitors.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Monitors     []Monitor
			MonitorHappy map[int]bool
			Ctx          pageCtx
		}{

			Monitors:     monitors,
			MonitorHappy: monitorHappy,
			Ctx:          getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("monitors.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getMonitorByID(tx *sql.Tx, id int) (Monitor, error) {
	const query = `
		select
			id,
			name,
			url,
			method,
			frequency,
			timeout,
			attempts,
			request_headers,
			body_format,
			body
		from
			monitor
		where
			id = ?
	`

	monitor := Monitor{}

	var serializedRequestHeaders sql.NullString

	err := tx.QueryRow(query, id).Scan(
		&monitor.ID,
		&monitor.Name,
		&monitor.URL,
		&monitor.Method,
		&monitor.Frequency,
		&monitor.Timeout,
		&monitor.Attempts,
		&serializedRequestHeaders,
		&monitor.BodyFormat,
		&monitor.Body,
	)
	if err != nil {
		return monitor, fmt.Errorf("getMonitorByID.QueryRow: %w", err)
	}

	requestHeaders := map[string]string{}

	if serializedRequestHeaders.Valid {
		err = json.Unmarshal([]byte(serializedRequestHeaders.String), &requestHeaders)
		if err != nil {
			return monitor, fmt.Errorf("getMonitorByID.Unmarshal: %w", err)
		}
	}

	monitor.RequestHeaders = requestHeaders

	return monitor, nil
}

type MonitorLog struct {
	ID           int
	StartedAt    time.Time
	EndedAt      time.Time
	ResponseCode sql.NullInt64
	ErrorMessage sql.NullString
	Attempts     int
	Result       string
	MonitorID    int
}

// monitorPollLimit caps the poll handler's query. High enough that a normal
// poll never notices; low enough that a crafted cursor cannot ask for a day.
const monitorPollLimit = 500
