package xormdriver

import (
	"github.com/zhoudm1743/go-fast-framework/contracts"
	"github.com/zhoudm1743/go-fast-framework/database"
	"github.com/zhoudm1743/go-fast-framework/foundation"
)

// ServiceProvider xorm 驱动接入点（可选驱动，需显式加入应用 providers）。
// 将本 Provider 加入应用 providers 后，配置 driver: "xorm" 即可使用：
//
//	import xormdriver "github.com/zhoudm1743/gofast-xorm"
//	app.SetProviders(append(providers, &xormdriver.ServiceProvider{}))
type ServiceProvider struct{}

func (sp *ServiceProvider) Register(app foundation.Application) {
	database.RegisterDriver("xorm", func(cfg database.ConnectionConfig, log contracts.Log) (contracts.Driver, error) {
		return NewXormDriver(cfg, log)
	})
}

func (sp *ServiceProvider) Boot(app foundation.Application) error {
	return nil
}
