package service

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/route"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"github.com/redis/go-redis/v9"
)

func NewRoutePublisherRunner(repository *store.Repository, runtimeConfig config.OrchestratorRuntimeConfig, onError func(error)) (orchestratorRunner, error) {
	if repository == nil || runtimeConfig.Validate() != nil {
		return nil, errors.New("execution route publisher configuration is invalid")
	}
	client := redis.NewClient(&redis.Options{Addr: runtimeConfig.RouteRedisAddr})
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := client.Ping(ping).Err()
	cancel()
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	publisher, err := route.NewPublisher(client, runtimeConfig.RoutePublishTTL)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	reconciler, err := route.NewReconciler(storeRouteSource{repository: repository}, publisher, time.Now)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	runner, err := route.NewRunner(reconciler, runtimeConfig.RoutePublishInterval, onError)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	return routePublisherProcess{runner: runner, client: client}, nil
}

type storeRouteSource struct {
	repository *store.Repository
}

func (s storeRouteSource) ListPublishableRoutes(ctx context.Context, checkedAt time.Time) ([]route.PublishableRoute, error) {
	items, err := s.repository.ListPublishableRoutes(ctx, checkedAt)
	if err != nil {
		return nil, err
	}
	routes := make([]route.PublishableRoute, 0, len(items))
	for _, item := range items {
		routes = append(routes, route.PublishableRoute{
			SlotID: item.SlotID, NodeID: item.NodeID, Endpoint: item.Endpoint,
			Epoch: item.Epoch, Generation: item.Generation,
		})
	}
	return routes, nil
}

type routePublisherProcess struct {
	runner *route.Runner
	client *redis.Client
}

func (p routePublisherProcess) Run(ctx context.Context) error {
	if p.client != nil {
		defer p.client.Close()
	}
	if p.runner == nil {
		return route.ErrRouteRunner
	}
	return p.runner.Run(ctx)
}
