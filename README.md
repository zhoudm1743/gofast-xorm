# gofast-xorm

GoFast 框架的 [xorm](https://xorm.io) 数据库驱动插件。

框架核心 `go-fast-framework` 不再内置 ORM 驱动，需显式安装并注册本插件。

## 安装

```bash
go get github.com/zhoudm1743/gofast-xorm@v0.8.2
```

## 接入

```go
import (
    "github.com/zhoudm1743/go-fast-framework/database"
    "github.com/zhoudm1743/go-fast-framework/foundation"
    xormdriver "github.com/zhoudm1743/gofast-xorm"
)

app.SetProviders([]foundation.ServiceProvider{
    // ...
    &database.ServiceProvider{},
    &xormdriver.ServiceProvider{},
})
```

```yaml
database:
  connections:
    main:
      driver: xorm
      engine: mysql   # mysql | postgres | sqlite | mssql
      # ...
```

## 依赖

- `github.com/zhoudm1743/go-fast-framework` >= v0.8.2
- `xorm.io/xorm` 及各 `database/sql` 驱动

## License

Apache-2.0
