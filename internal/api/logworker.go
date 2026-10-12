package api

import (
	"context"
	"log"
	"time"

	"llm-api/internal/domain"
	"llm-api/internal/store"
)

// logWorker asynchronously persists request logs so billing inserts don't add
// latency to responses.
type logWorker struct {
	ch   chan *domain.RequestLog
	done chan struct{}
	ctx  context.Context
	st   *store.Store
}

func newLogWorker(ctx context.Context, st *store.Store) *logWorker {
	lw := &logWorker{
		ch:   make(chan *domain.RequestLog, 2048),
		done: make(chan struct{}),
		ctx:  ctx,
		st:   st,
	}
	go lw.run()
	return lw
}

func (lw *logWorker) submit(l *domain.RequestLog) {
	select {
	case lw.ch <- l:
	default:
		// queue full: insert synchronously to avoid data loss
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := lw.st.InsertLog(ctx, l); err != nil {
			log.Printf("[log] insert failed (overflow): %v", err)
		}
	}
}

func (lw *logWorker) run() {
	defer close(lw.done)
	for {
		select {
		case <-lw.ctx.Done():
			// drain remaining on shutdown
			for {
				select {
				case l := <-lw.ch:
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					if err := lw.st.InsertLog(ctx, l); err != nil {
						log.Printf("[log] insert failed (shutdown): %v", err)
					}
					cancel()
				default:
					return
				}
			}
		case l := <-lw.ch:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := lw.st.InsertLog(ctx, l); err != nil {
				log.Printf("[log] insert failed: %v", err)
			}
			cancel()
		}
	}
}
