package service

import (
	"context"
	"github.com/lejianwen/rustdesk-api/v2/model"
	"gorm.io/gorm"
)

type OperationLogService struct {
	db *gorm.DB
}

func NewOperationLogService(db *gorm.DB) *OperationLogService { return &OperationLogService{db: db} }

func (os *OperationLogService) CreateContext(ctx context.Context, record *model.OperationLog) error {
	return os.db.WithContext(ctx).Create(record).Error
}

// Create 创建操作日志
func (os *OperationLogService) Create(log *model.OperationLog) error {
	return DB.Create(log).Error
}

// List 列表
func (os *OperationLogService) List(page, pageSize uint, where func(tx *gorm.DB)) (res *model.OperationLogList) {
	res = &model.OperationLogList{}
	res.Page = int64(page)
	res.PageSize = int64(pageSize)
	tx := DB.Model(&model.OperationLog{})
	if where != nil {
		where(tx)
	}
	tx.Count(&res.Total)
	tx.Scopes(Paginate(page, pageSize))
	tx.Order("id desc")
	tx.Find(&res.OperationLogs)
	return
}
