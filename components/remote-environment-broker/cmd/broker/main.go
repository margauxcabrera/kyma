package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	scCs "github.com/kubernetes-incubator/service-catalog/pkg/client/clientset_generated/clientset"
	catalogInformers "github.com/kubernetes-incubator/service-catalog/pkg/client/informers_generated/externalversions"
	"github.com/kubernetes-incubator/service-catalog/pkg/svcat/service-catalog"
	"github.com/kyma-project/kyma/components/remote-environment-broker/internal/access"
	"github.com/kyma-project/kyma/components/remote-environment-broker/internal/broker"
	"github.com/kyma-project/kyma/components/remote-environment-broker/internal/config"
	"github.com/kyma-project/kyma/components/remote-environment-broker/internal/labeler"
	"github.com/kyma-project/kyma/components/remote-environment-broker/internal/mode"
	"github.com/kyma-project/kyma/components/remote-environment-broker/internal/storage"
	"github.com/kyma-project/kyma/components/remote-environment-broker/internal/storage/populator"
	"github.com/kyma-project/kyma/components/remote-environment-broker/internal/syncer"
	"github.com/kyma-project/kyma/components/remote-environment-broker/pkg/client/clientset/versioned"
	"github.com/kyma-project/kyma/components/remote-environment-broker/pkg/client/informers/externalversions"
	"github.com/kyma-project/kyma/components/remote-environment-broker/platform/logger"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// informerResyncPeriod defines how often informer will execute relist action. Setting to zero disable resync.
// BEWARE: too short period time will increase the CPU load.
const informerResyncPeriod = 30 * time.Minute

func main() {
	verbose := flag.Bool("verbose", false, "specify if log verbosely loading configuration")
	flag.Parse()
	cfg, err := config.Load(*verbose)
	fatalOnError(err)

	log := logger.New(&cfg.Logger)

	// create storage factory
	storageConfig := storage.ConfigList(cfg.Storage)
	sFact, err := storage.NewFactory(&storageConfig)
	fatalOnError(err)

	k8sConfig, err := restclient.InClusterConfig()
	fatalOnError(err)

	// k8s
	k8sClient, err := kubernetes.NewForConfig(k8sConfig)
	fatalOnError(err)
	nsInformer := v1.NewNamespaceInformer(k8sClient, informerResyncPeriod, cache.Indexers{})

	// ServiceCatalog
	scClientSet, err := scCs.NewForConfig(k8sConfig)
	fatalOnError(err)
	scSDK := &servicecatalog.SDK{ServiceCatalogClient: scClientSet}

	scInformerFactory := catalogInformers.NewSharedInformerFactory(scClientSet, informerResyncPeriod)
	scInformersGroup := scInformerFactory.Servicecatalog().V1beta1()

	// instance populator
	instancePopulator := populator.NewInstances(scClientSet, sFact.Instance(), cfg.ClusterScopedBrokerName)
	popCtx, popCancelFunc := context.WithTimeout(context.Background(), time.Minute)
	defer popCancelFunc()
	log.Info("Instance storage population...")
	err = instancePopulator.Do(popCtx)
	fatalOnError(err)
	log.Info("Instance storage populated")

	// RemoteEnvironments
	reClient, err := versioned.NewForConfig(k8sConfig)
	fatalOnError(err)
	reInformerFactory := externalversions.NewSharedInformerFactory(reClient, informerResyncPeriod)
	reInformersGroup := reInformerFactory.Remoteenvironment().V1alpha1()

	// internal services
	relistRequester := syncer.NewRelistRequester(scSDK, cfg.ClusterScopedBrokerName, cfg.BrokerRelistDurationWindow, log)
	siFacade := broker.NewServiceInstanceFacade(scInformersGroup.ServiceInstances().Informer())
	accessChecker := access.New(sFact.RemoteEnvironment(), reClient.RemoteenvironmentV1alpha1(), sFact.Instance())

	reSyncCtrl := syncer.New(reInformersGroup.RemoteEnvironments(), sFact.RemoteEnvironment(), sFact.RemoteEnvironment(), relistRequester, log)
	labelerCtrl := labeler.New(reInformersGroup.EnvironmentMappings().Informer(), nsInformer, k8sClient.CoreV1().Namespaces(), sFact.RemoteEnvironment(), log)

	brokerMode, err := mode.NewBrokerService(cfg)
	fatalOnError(err)
	// create broker
	srv := broker.New(sFact.RemoteEnvironment(), sFact.Instance(), sFact.InstanceOperation(), accessChecker,
		reClient.RemoteenvironmentV1alpha1(), siFacade, brokerMode, log)

	// setup graceful shutdown signals
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	stopCh := make(chan struct{})
	cancelOnInterrupt(ctx, stopCh, cancelFunc)

	// start informers
	scInformerFactory.Start(stopCh)
	reInformerFactory.Start(stopCh)
	go nsInformer.Run(stopCh)

	// wait for cache sync
	scInformerFactory.WaitForCacheSync(stopCh)
	reInformerFactory.WaitForCacheSync(stopCh)
	cache.WaitForCacheSync(stopCh, nsInformer.HasSynced)

	// start services & ctrl
	go reSyncCtrl.Run(stopCh)
	go labelerCtrl.Run(stopCh)
	go relistRequester.Run(stopCh)

	fatalOnError(srv.Run(ctx, fmt.Sprintf(":%d", cfg.Port)))
}

func fatalOnError(err error) {
	if err != nil {
		logrus.Fatal(err.Error())
	}
}

// cancelOnInterrupt closes given channel and also calls cancel func when os.Interrupt or SIGTERM is received
func cancelOnInterrupt(ctx context.Context, ch chan<- struct{}, cancel context.CancelFunc) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ctx.Done():
			close(ch)
		case <-c:
			close(ch)
			cancel()
		}
	}()
}
