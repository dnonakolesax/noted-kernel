# Установка модулей
FROM golang:1.25.3 as modules

ADD go.mod go.sum /m/
RUN cd /m && go mod download

# Сборка приложения
FROM golang:1.25.3 as builder

COPY --from=modules /go/pkg /go/pkg

# Пользователь без прав
RUN useradd -u 10001 nkernel-runner

RUN mkdir -p /noted-kernel
ADD . /noted-kernel
WORKDIR /noted-kernel

# Сборка
RUN GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -o ./bin/noted-kernel main.go

# Запуск в пустом контейнере
FROM scratch

# Копируем пользователя без прав с прошлого этапа
COPY --from=builder /etc/passwd /etc/passwd
# Запускаем от имени этого пользователя
USER nkernel-runner

COPY --from=builder /noted-kernel/bin/noted-kernel /noted-kernel

CMD ["/noted-kernel"]
