package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxMonitorResponse caps how much of a monitored endpoint's response body is
// read. The body is discarded; only the status code matters.
const maxMonitorResponse = 1 << 20

func monitorLoop(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	checkoutMu := sync.RWMutex{}
	lastCheckedMu := sync.RWMutex{}

	lastChecked := map[int]time.Time{}
	checkout := map[int]time.Time{}

	tick := time.Tick(time.Millisecond * 500)

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
						// This monitor is still being checked; skip it, but
						// keep checking the rest of the list.
						checkoutMu.RUnlock()
						continue
					}
					checkoutMu.RUnlock()

					checkoutMu.Lock()
					checkout[monitor.ID] = time.Now().UTC()
					checkoutMu.Unlock()

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

							resp, reqErr = httpClient.Do(monitorReq)
							if reqErr != nil {
								continue
							}

							_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMonitorResponse))
							if err != nil {
								log.Printf("monitorLoop.Copy: %s", err)
								break
							}

							resp.Body.Close()

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
								result = "timeout"
							}
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
							attempt,
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

						// lastChecked.ID is zero when this is the monitor's first
						// ever check: there is no transition to report, and
						// notifying here announced a recovery that never happened.
						if len(channels) > 0 && lastChecked.ID != 0 {
							lastHappy := lastChecked.ResponseCode.Int32 != 0 &&
								lastChecked.ResponseCode.Int32 < 400

							if lastHappy && result != "success" || !lastHappy && result == "success" {
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
											metaDomain,
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
			return
		}
	}
}
