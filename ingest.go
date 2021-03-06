package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/packethost/packngo"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
)

func fetchFacilityPage(ctx context.Context, client *packngo.Client, url string) ([]map[string]interface{}, uint, uint, error) {
	req, err := client.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, 0, errors.Wrap(err, "failed to create fetch request")
	}
	req = req.WithContext(ctx)
	req.Header.Add("X-Packet-Staff", "true")

	r := struct {
		Meta struct {
			CurrentPage int `json:"current_page"`
			LastPage    int `json:"last_page"`
			Total       int `json:"total"`
		}
		Hardware []map[string]interface{}
	}{}
	_, err = client.Do(req, &r)
	if err != nil {
		return nil, 0, 0, errors.Wrap(err, "failed to fetch page")
	}

	return r.Hardware, uint(r.Meta.LastPage), uint(r.Meta.Total), nil
}

func fetchFacility(ctx context.Context, client *packngo.Client, api *url.URL, facility string, data chan<- []map[string]interface{}) error {
	logger.Info("fetch start")
	labels := prometheus.Labels{"method": "Ingest", "op": "fetch"}
	ingestCount.With(labels).Inc()
	timer := prometheus.NewTimer(prometheus.ObserverFunc(ingestDuration.With(labels).Set))

	defer close(data)

	api.Path = "/staff/cacher/hardware"
	q := api.Query()
	q.Set("facility", facility)
	q.Set("sort_by", "created_at")
	q.Set("sort_direction", "asc")
	q.Set("per_page", "50")

	have := 0
	for page, lastPage := uint(1), uint(1); page <= lastPage; page++ {
		q.Set("page", strconv.Itoa(int(page)))
		api.RawQuery = q.Encode()
		hw, last, total, err := fetchFacilityPage(ctx, client, api.String())
		if err != nil {
			return errors.Wrapf(err, "failed to fetch page")
		}
		lastPage = last
		have += len(hw)
		logger.With("have", have, "want", total).Info("fetched a page")
		data <- hw
	}

	timer.ObserveDuration()
	logger.Info("fetch done")

	return nil
}

func copyin(ctx context.Context, db *sql.DB, data <-chan []map[string]interface{}) error {
	for hws := range data {
		if err := copyInEach(ctx, db, hws); err != nil {
			return err
		}
	}

	return nil
}

func copyInEach(ctx context.Context, db *sql.DB, data []map[string]interface{}) error {
	logger.Info("copy start")
	labels := prometheus.Labels{"method": "Ingest", "op": "copy"}
	ingestCount.With(labels).Inc()
	timer := prometheus.NewTimer(prometheus.ObserverFunc(ingestDuration.With(labels).Set))

	now := time.Now()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return errors.Wrap(err, "BEGIN transaction")
	}

	stmt, err := tx.Prepare(`
	INSERT INTO
		hardware (data)
	VALUES
		($1)
	`)

	if err != nil {
		return errors.Wrap(err, "PREPARE INSERT")
	}

	for _, j := range data {
		var q []byte
		q, err = json.Marshal(j)
		if err != nil {
			return errors.Wrap(err, "marshal json")
		}
		_, err = stmt.Exec(q)
		if err != nil {
			return errors.Wrap(err, "INSERT")
		}
	}

	err = stmt.Close()
	if err != nil {
		return errors.Wrap(err, "Close")
	}

	// Remove duplicates, keeping what has already been inserted via insertIntoDB since startup
	_, err = tx.Exec(`
	DELETE FROM hardware a
	USING hardware b
	WHERE a.id IS NULL
	AND (a.data ->> 'id')::uuid = b.id
	`)
	if err != nil {
		return errors.Wrap(err, "delete overwrite")
	}

	_, err = tx.Exec(`
	UPDATE hardware
	SET (inserted_at, id) =
	  ($1::timestamptz, (data ->> 'id')::uuid);
	`, now)
	if err != nil {
		return errors.Wrap(err, "set inserted_at and id")
	}

	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "COMMIT")
	}

	_, err = db.Exec("VACUUM FULL ANALYZE")
	if err != nil {
		return errors.Wrap(err, "VACCUM FULL ANALYZE")
	}

	timer.ObserveDuration()
	logger.Info("copy done")

	return nil
}

func (s *server) ingest(ctx context.Context, api *url.URL, facility string) error {
	logger.Info("ingestion is starting")
	defer logger.Info("ingestion is done")

	labels := prometheus.Labels{"method": "Ingest", "op": ""}
	cacheInFlight.With(labels).Inc()
	defer cacheInFlight.With(labels).Dec()

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)

	ch := make(chan []map[string]interface{}, 1)
	errCh := make(chan error)
	go func() {
		defer wg.Done()

		if err := fetchFacility(ctx, s.packet, api, facility, ch); err != nil {
			labels := prometheus.Labels{"method": "Ingest", "op": "fetch"}
			ingestErrors.With(labels).Inc()
			logger.With("error", err).Info()

			if ctx.Err() == context.Canceled {
				return
			}

			cancel()
			errCh <- err
		}
	}()
	go func() {
		defer wg.Done()

		if err := copyin(ctx, s.db, ch); err != nil {
			labels := prometheus.Labels{"method": "Ingest", "op": "copy"}
			ingestErrors.With(labels).Inc()

			l := logger.With("error", err)
			if pqErr := pqError(err); pqErr != nil {
				l = l.With("detail", pqErr.Detail, "where", pqErr.Where)
			}
			l.Info()

			if ctx.Err() == context.Canceled {
				return
			}

			cancel()
			errCh <- err
		}
	}()

	wg.Wait()
	cancel()

	select {
	case err := <-errCh:
		return err
	default:
	}

	s.dbLock.Lock()
	s.dbReady = true
	s.dbLock.Unlock()

	return nil
}
