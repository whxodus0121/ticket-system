package repository

import (
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Ticket 도메인 모델
type Ticket struct {
	ID    uint `gorm:"primaryKey"`
	Name  string
	Stock int
}

// Purchase 구매 내역 모델 (최종 데이터 영속화용)
type Purchase struct {
	ID         uint      `gorm:"primaryKey;autoIncrement"`
	UserID     string    `gorm:"column:user_id;not null"`
	TicketName string    `gorm:"column:ticket_name;not null"`
	CreatedAt  time.Time `gorm:"column:created_at;autoCreateTime"`
}

func (Purchase) TableName() string {
	return "purchases"
}

type MySQLRepository struct {
	DB *gorm.DB
}

func NewMySQLRepository(db *gorm.DB) *MySQLRepository {
	return &MySQLRepository{
		DB: db,
	}
}

// DecreaseStock: DB 수준의 원자적 재고 차감을 수행 (Redis 장애 대비용)
func (r *MySQLRepository) DecreaseStock(name string) error {
	// stock > 0 일 때만 1을 깎는 안전한 쿼리
	return r.DB.Model(&Ticket{}).
		Where("name = ? AND stock > 0", name).
		Update("stock", gorm.Expr("stock - 1")).Error
}

// 현재 재고 확인 (SELECT 쿼리 실행)
func (r *MySQLRepository) GetStock(name string) (int, error) {
	var ticket Ticket
	err := r.DB.Where("name = ?", name).First(&ticket).Error
	return ticket.Stock, err
}

// SavePurchase: 중복 구매 방지를 위해 OnConflict(Ignore) 전략을 사용하여 구매 내역을 저장
func (r *MySQLRepository) SavePurchase(userID string, ticketName string) (bool, error) {
	result := r.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&Purchase{
		UserID:     userID,
		TicketName: ticketName,
	})

	if result.Error != nil {
		return false, result.Error
	}

	return result.RowsAffected > 0, nil
}

func (r *MySQLRepository) ExistsPurchase(userID string, ticketName string) (bool, error) {
	var count int64
	// purchases 테이블에서 해당 유저와 티켓이 있는지 COUNT를 확인
	err := r.DB.Table("purchases").
		Where("user_id = ? AND ticket_name = ?", userID, ticketName).
		Count(&count).Error

	return count > 0, err
}

func (r *MySQLRepository) DeletePurchase(userID string, ticketName string) error {
	// GORM을 사용하여 조건에 맞는 데이터를 삭제
	// Unscoped()를 붙이지 않으면 Soft Delete가 설정된 경우 실제 삭제가 안 될 수 있으므로 확실히 지우기 위해 사용
	result := r.DB.Unscoped().Where("user_id = ? AND ticket_name = ?", userID, ticketName).Delete(&Purchase{})

	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected == 0 {
		return fmt.Errorf("취소할 내역이 없습니다 (유저: %s)", userID)
	}

	return nil
}
