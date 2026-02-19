package models

import (
	"gorm.io/gorm"
)

type User struct {
	gorm.Model
	Username string `gorm:"unique;not null"`
	Password string `gorm:"not null"`
	Nickname string
	Avatar   string
}

type Note struct {
	gorm.Model
	Title     string `gorm:"type:varchar(100);not null"`
	Content   string `gorm:"type:text;not null"`
	ImgURL    string // 存储图片在 OSS 的访问地址
	UserID    uint
	LikeCount uint
}

type Follow struct {
	gorm.Model
	FollowerID uint `gorm:"index"`
	FollowedID uint `gorm:"index"`
}
