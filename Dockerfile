# Build stage
FROM golang:1.20 AS builder
WORKDIR /app
# 复制模块文件，如果 go.sum 不存在，请先执行 go mod tidy 生成
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 禁用 cgo，使编译生成的二进制文件静态链接，适合在 Alpine 上运行
RUN CGO_ENABLED=0 go build -o chatserver

# Run stage
FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/chatserver .
EXPOSE 8080
CMD ["./chatserver"]
