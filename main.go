package main

import (
	"context"
	"cuplore/middleware"
	"cuplore/models"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/redis/go-redis/v9"
)

var db *gorm.DB
var bucketName = "red-book"
var minioClient *minio.Client

var rdb *redis.Client
var ctx = context.Background()

func main() {
	initMinio()
	initRedis()
	dsn := "root:123456@tcp(127.0.0.1:3306)/red_book?charset=utf8mb4&parseTime=True&loc=Local"
	var err error
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		panic("Connection Failed" + err.Error())
	}

	db.AutoMigrate(&models.User{}, &models.Note{}, &models.Follow{})

	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		for range ticker.C {
			SyncRedisToMySQL()
		}
	}()

	SyncChannelToMySQL()

	r := gin.Default()
	r.POST("/register", register)
	r.POST("/login", login)

	auth := r.Group("/")
	auth.Use(middleware.AuthMiddleware())
	{
		auth.GET("/me", getMe)
		auth.POST("/notes", createNote)
		auth.POST("/upload", uploadImage)
		auth.POST("/notes/:id/like", likeNote)
		auth.POST("/follow/:id", FollowUser)
	}

	r.Run(":8080")
}

var followTaskChannel = make(chan models.Follow, 1000)

func FollowUser(c *gin.Context) {
	targetID, _ := strconv.Atoi(c.Param("id"))
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(500, gin.H{"error": "未登录"})
		return
	}

	f := models.Follow{
		FollowerID: userID.(uint),
		FollowedID: uint(targetID),
	}

	select {
	case followTaskChannel <- f:
		c.JSON(200, gin.H{"message": "关注成功"})
	default:
		c.JSON(503, gin.H{"message": "系统繁忙"})
	}
}

func likeNote(c *gin.Context) {
	noteID := c.Param("id") //Param 拿到路由中的dynamic参数
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(500, gin.H{"error": "未登录"})
		return
	}

	key := "notes:likes:" + noteID

	val, err := rdb.SAdd(ctx, key, userID).Result()

	if err != nil {
		c.JSON(500, gin.H{"error": "Redis wrong"})
		return
	}

	if val == 0 {
		_, err = rdb.SRem(ctx, key, userID).Result()
		if err != nil {
			c.JSON(500, gin.H{"error": "Redis wrong"})
			return
		}
		c.JSON(200, gin.H{"message": "取消点赞"})
		return
	}
	c.JSON(200, gin.H{"message": "点赞成功"})
}

func register(c *gin.Context) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	c.ShouldBindJSON(&input) // 存前端传入的代码到input

	hashedPassword, _ := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	newUser := models.User{
		Username: input.Username,
		Password: string(hashedPassword),
		Nickname: "Hola",
	}

	db.Create(&newUser)
	c.JSON(200, gin.H{
		"message": "register success",
		"id":      newUser.ID,
	})
}

func login(c *gin.Context) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	c.ShouldBindJSON(&input)

	var user models.User
	err := db.Where("username = ?", input.Username).First(&user).Error

	if err != nil {
		c.JSON(400, gin.H{"message": "不存在"})
		return
	}

	err = bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(input.Password))

	if err != nil {
		c.JSON(400, gin.H{"message": "不匹配"})
		return
	}

	// Distribute pass
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": user.ID,
		"exp":     time.Now().Add(time.Hour * 24).Unix(),
	})

	tokenString, _ := token.SignedString(middleware.JwtKey)

	c.JSON(200, gin.H{
		"message": "登录成功",
		"token":   tokenString,
	})
}

func uploadImage(c *gin.Context) {
	file, err := c.FormFile("image") //前端需要在表单中用字段名 image 上传文件：
	if err != nil {
		c.JSON(400, gin.H{"error": "Failed to get files"})
		return
	}
	src, err := file.Open()
	if err != nil {
		c.JSON(400, gin.H{"error": "Failed to open file"})
		return
	}
	defer src.Close()

	// generate unique id

	ext := filepath.Ext(file.Filename)
	objectName := uuid.New().String() + ext
	contentType := file.Header.Get("Content-Type")
	// 4. 上传到 MinIO
	_, err = minioClient.PutObject(context.Background(), bucketName, objectName, src, file.Size, minio.PutObjectOptions{
		ContentType: contentType,
	})

	if err != nil {
		c.JSON(500, gin.H{
			"error": "Failed to upload Minio: " + err.Error(),
		})
		return
	}

	url := "http://127.0.0.1:9000/" + bucketName + "/" + objectName

	c.JSON(200, gin.H{
		"message": "Upload Success",
		"url":     url,
	})
}

func createNote(c *gin.Context) {
	var input struct {
		Title   string `json:"title"`
		Content string `json:"content"`
		ImgURL  string `json:"img_url"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(400, gin.H{"error": "parameters wrong"})
		return
	}

	uid, _ := c.Get("user_id")
	newNote := models.Note{
		Title:   input.Title,
		Content: input.Content,
		ImgURL:  input.ImgURL,
		UserID:  uid.(uint),
	}
	if err := db.Create(&newNote).Error; err != nil {
		c.JSON(500, gin.H{"error": "Deploy Failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Deploy Succeed",
		"note_id": newNote.ID,
	})
}

func getMe(c *gin.Context) {
	uid, _ := c.Get("user_id")

	c.JSON(200, gin.H{
		"message": "个人详情",
		"your_id": uid,
	})

	// c.JSON(200, map[string]interface{}{
	// 	"message": "detail",
	// 	"uid":     userID,
	// })
}

func initMinio() {
	endpoint := "127.0.0.1:9000"
	accessKeyID := "admin"
	accessKey := "password123"
	useSSL := false

	var err error
	minioClient, err = minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKeyID, accessKey, ""),
		Secure: useSSL,
	})

	if err != nil {
		panic("Failed to connect Minio:" + err.Error())
	}
}

func initRedis() {
	rdb = redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "",
		DB:       0,
	})
}

func SyncRedisToMySQL() {
	keys, err := rdb.Keys(ctx, "notes:likes:*").Result()
	if err != nil {
		slog.Error("Failed to get Redis keys",
			"error", err,
			"pattern", "notes:likes:*",
		)
		return
	}
	for _, key := range keys { //range 返回两个值：第一个是索引 (Index)，第二个是元素值 (Value)。
		noteID := strings.TrimPrefix(key, "notes:likes:")

		count, err := rdb.SCard(ctx, key).Result()

		if err != nil {
			continue //防御性编程
		}
		db.Model(&models.Note{}).Where("id = ?", noteID).Update("like_count", count)
	}

	fmt.Printf("定时同步已完成，时间: %s, 同步数量: %d\n", time.Now().Format("15:04:05"), len(keys))
}

func SyncChannelToMySQL() {
	go func() {
		for f := range followTaskChannel {
			db.Create(&f)
			fmt.Println("异步写入数据库成功")
		}
	}()

	/*
		go someFunction()        // 调用有名函数
		go func() { ... }()      // 调用匿名函数

		// 定义 + 立即调用
		func() {
			fmt.Println("Hello")
		}()  // ← 这个 () 表示“调用”
	*/

}
