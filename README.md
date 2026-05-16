# Transmission VK Bot

VK-бот на Go для управления торрентами Transmission через мессенджер.

## Возможности

- Подключение к локальному Transmission daemon через RPC
- Добавление торрентов по magnet-ссылкам
- Мониторинг статуса загрузки
- Управление загрузками через VK

## Требования

- Go 1.21+
- Transmission daemon с включённым RPC интерфейсом
- VK API токен (для мессенджера)
- [opencode](https://opencode.ai) бинарник (опционально, для LLM функций)

## Установка

1. Клонировать репозиторий:
```bash
git clone https://github.com/Grigory-Rylov/transmission-vk-bot.git
cd transmission-vk-bot
```

2. Собрать проект:
```bash
go build -o bot .
```

3. Скопировать и настроить конфиг:
```bash
cp config.json.example config.json
# Отредактировать config.json, указав свои данные
```

## Команды бота

| Команда | Описание |
|---------|----------|
| `/add <magnet-link>` | Добавить торрент для загрузки |
| `/status` | Показать статус всех загрузок |
| `/help` | Показать справку с командами |

## Конфигурация

Скопируйте `config.json.example` в `config.json` и заполните значения:

```json
{
  "vk_token": "ваш_vk_api_token",
  "vk_api_version": "5.200",
  "long_poll_wait": 25,
  "transmission": {
    "url": "http://192.168.0.192:9091/transmission/rpc",
    "username": "transmission",
    "password": "your_password",
    "default_folder": "/home/user/Downloads"
  }
}
```

### Параметры Transmission

| Параметр | Описание |
|----------|----------|
| `url` | URL RPC интерфейса Transmission |
| `username` | Имя пользователя для аутентификации |
| `password` | Пароль для аутентификации |
| `default_folder` | Папка по умолчанию для загрузки торрентов |

### Структура проекта

| Файл | Назначение |
|------|-----------|
| `main.go` | Точка входа, VK логика |
| `transmission/client.go` | Transmission RPC клиент |
| `transmission/types.go` | Типы данных |
| `transmission/errors.go` | Обработка ошибок |
| `config.json` | Конфигурация (не в git) |
| `config.json.example` | Шаблон конфигурации |

## Запуск

```bash
./bot
```

## License

MIT
