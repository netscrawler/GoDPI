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

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -buildid= -extldflags=-static" \
    -buildvcs=false \
    -o godpi ./cmd/dpi/main.go

FROM gcr.io/distroless/static-debian12

WORKDIR /godpi

COPY --from=builder /app/godpi .

COPY --from=builder /app/blacklist.txt .

EXPOSE 8881

CMD ["./godpi"]

