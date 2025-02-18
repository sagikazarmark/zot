//go:build search
// +build search

package extensions

import (
	"context"
	"net/http"
	"sync"
	"time"

	gqlHandler "github.com/99designs/gqlgen/graphql/handler"
	"github.com/gorilla/mux"

	"zotregistry.io/zot/pkg/api/config"
	"zotregistry.io/zot/pkg/api/constants"
	zcommon "zotregistry.io/zot/pkg/common"
	"zotregistry.io/zot/pkg/extensions/search"
	cveinfo "zotregistry.io/zot/pkg/extensions/search/cve"
	"zotregistry.io/zot/pkg/extensions/search/gql_generated"
	"zotregistry.io/zot/pkg/log"
	mTypes "zotregistry.io/zot/pkg/meta/types"
	"zotregistry.io/zot/pkg/scheduler"
	"zotregistry.io/zot/pkg/storage"
)

type (
	CveInfo cveinfo.CveInfo
	state   int
)

const (
	pending state = iota
	running
	done
)

func IsBuiltWithSearchExtension() bool {
	return true
}

func GetCVEInfo(config *config.Config, storeController storage.StoreController,
	metaDB mTypes.MetaDB, log log.Logger,
) CveInfo {
	if config.Extensions.Search == nil || !*config.Extensions.Search.Enable || config.Extensions.Search.CVE == nil {
		return nil
	}

	dbRepository := config.Extensions.Search.CVE.Trivy.DBRepository
	javaDBRepository := config.Extensions.Search.CVE.Trivy.JavaDBRepository

	return cveinfo.NewCVEInfo(storeController, metaDB, dbRepository, javaDBRepository, log)
}

func EnableSearchExtension(config *config.Config, storeController storage.StoreController,
	metaDB mTypes.MetaDB, taskScheduler *scheduler.Scheduler, cveInfo CveInfo, log log.Logger,
) {
	if config.Extensions.Search != nil && *config.Extensions.Search.Enable && config.Extensions.Search.CVE != nil {
		updateInterval := config.Extensions.Search.CVE.UpdateInterval

		downloadTrivyDB(updateInterval, taskScheduler, cveInfo, log)
	} else {
		log.Info().Msg("CVE config not provided, skipping CVE update")
	}
}

func downloadTrivyDB(interval time.Duration, sch *scheduler.Scheduler, cveInfo CveInfo, log log.Logger) {
	generator := NewTrivyTaskGenerator(interval, cveInfo, log)

	log.Info().Msg("Submitting CVE DB update scheduler")
	sch.SubmitGenerator(generator, interval, scheduler.HighPriority)
}

func NewTrivyTaskGenerator(interval time.Duration, cveInfo CveInfo, log log.Logger) *TrivyTaskGenerator {
	generator := &TrivyTaskGenerator{interval, cveInfo, log, pending, 0, time.Now(), &sync.Mutex{}}

	return generator
}

type TrivyTaskGenerator struct {
	interval     time.Duration
	cveInfo      CveInfo
	log          log.Logger
	status       state
	waitTime     time.Duration
	lastTaskTime time.Time
	lock         *sync.Mutex
}

func (gen *TrivyTaskGenerator) Next() (scheduler.Task, error) {
	var newTask scheduler.Task

	gen.lock.Lock()

	if gen.status == pending && time.Since(gen.lastTaskTime) >= gen.waitTime {
		newTask = newTrivyTask(gen.interval, gen.cveInfo, gen, gen.log)
		gen.status = running
	}
	gen.lock.Unlock()

	return newTask, nil
}

func (gen *TrivyTaskGenerator) IsDone() bool {
	gen.lock.Lock()
	status := gen.status
	gen.lock.Unlock()

	return status == done
}

func (gen *TrivyTaskGenerator) IsReady() bool {
	return true
}

func (gen *TrivyTaskGenerator) Reset() {
	gen.lock.Lock()
	gen.status = pending
	gen.waitTime = 0
	gen.lock.Unlock()
}

type trivyTask struct {
	interval  time.Duration
	cveInfo   cveinfo.CveInfo
	generator *TrivyTaskGenerator
	log       log.Logger
}

func newTrivyTask(interval time.Duration, cveInfo cveinfo.CveInfo,
	generator *TrivyTaskGenerator, log log.Logger,
) *trivyTask {
	return &trivyTask{interval, cveInfo, generator, log}
}

func (trivyT *trivyTask) DoWork(ctx context.Context) error {
	trivyT.log.Info().Msg("updating the CVE database")

	err := trivyT.cveInfo.UpdateDB()
	if err != nil {
		trivyT.generator.lock.Lock()
		trivyT.generator.status = pending

		if trivyT.generator.waitTime == 0 {
			trivyT.generator.waitTime = time.Second
		}

		trivyT.generator.waitTime *= 2
		trivyT.generator.lastTaskTime = time.Now()
		trivyT.generator.lock.Unlock()

		return err
	}

	trivyT.generator.lock.Lock()
	trivyT.generator.lastTaskTime = time.Now()
	trivyT.generator.status = done
	trivyT.generator.lock.Unlock()
	trivyT.log.Info().Str("DB update completed, next update scheduled after", trivyT.interval.String()).Msg("")

	return nil
}

func SetupSearchRoutes(conf *config.Config, router *mux.Router, storeController storage.StoreController,
	metaDB mTypes.MetaDB, cveInfo CveInfo, log log.Logger,
) {
	if !conf.IsSearchEnabled() {
		log.Info().Msg("skip enabling the search route as the config prerequisites are not met")

		return
	}

	log.Info().Msg("setting up search routes")

	resConfig := search.GetResolverConfig(log, storeController, metaDB, cveInfo)

	allowedMethods := zcommon.AllowedMethods(http.MethodGet, http.MethodPost)

	extRouter := router.PathPrefix(constants.ExtSearchPrefix).Subrouter()
	extRouter.Use(zcommon.CORSHeadersMiddleware(conf.HTTP.AllowOrigin))
	extRouter.Use(zcommon.ACHeadersMiddleware(conf, allowedMethods...))
	extRouter.Use(zcommon.AddExtensionSecurityHeaders())
	extRouter.Methods(allowedMethods...).
		Handler(gqlHandler.NewDefaultServer(gql_generated.NewExecutableSchema(resConfig)))

	log.Info().Msg("finished setting up search routes")
}
