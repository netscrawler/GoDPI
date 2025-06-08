# Используем официальный образ Go как базовый для сборки
FROM golang:1.24 AS builder

# Устанавливаем рабочую директорию
WORKDIR /app

# Копируем go.mod и go.sum для кэширования зависимостей
COPY go.mod go.sum ./

# Загружаем зависимости
RUN go mod download

# Копируем весь исходный код
COPY . .

# Создаем папку target
RUN mkdir -p target

# Компилируем бинарники для разных систем
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o target/GoDPI-linux ./cmd/dpi/main.go
RUN CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o target/GoDPI-windows.exe ./cmd/dpi/main.go
RUN CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o target/GoDPI-darwin ./cmd/dpi/main.go

# Финальный этап
FROM alpine:latest

# Копируем папку target из стадии builder
COPY --from=builder /app/target /target

# Указываем фиктивную команду, чтобы docker create работал
CMD ["sh"]
