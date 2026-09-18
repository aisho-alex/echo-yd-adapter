#!/usr/bin/env python3
"""A3: smoke-проверка Yandex Disk REST API (одноразовый скрипт разведки).

Создаёт /echo/smoke, загружает hello.txt (overwrite=false), читает список,
скачивает и сравнивает, удаляет папку. Отдельно проверяет повторную загрузку
по занятому имени (ожидаем 409/423) и режим --rate для замера лимитов (A4).

Запуск:  TOKEN=... python3 tools/yd_smoke.py [--rate N]
Зависимости: только стандартная библиотека.
"""
import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

API = "https://cloud-api.yandex.net/v1/disk"
SMOKE_DIR = "/echo/smoke"
HELLO = "Привет из smoke-теста файлового транспорта.\n"


def req(token: str, method: str, url: str, data: bytes | None = None):
    r = urllib.request.Request(url, data=data, method=method)
    r.add_header("Authorization", f"OAuth {token}")
    if data is not None:
        r.add_header("Content-Type", "application/octet-stream")
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            body = resp.read()
            return resp.status, body
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def get_upload_href(token: str, path: str, overwrite: bool = False):
    st, body = req(token, "GET",
                   f"{API}/resources/upload?path={urllib.parse.quote(path)}"
                   f"&overwrite={'true' if overwrite else 'false'}")
    if st != 200:
        return st, None
    return st, json.loads(body).get("href")


def upload(token: str, path: str, data: bytes, overwrite: bool = False):
    st, href = get_upload_href(token, path, overwrite)
    if href is None:
        return st, b""
    return req(token, "PUT", href, data)


def download(token: str, path: str):
    st, body = req(token, "GET",
                   f"{API}/resources/download?path={urllib.parse.quote(path)}")
    if st != 200:
        return st, b""
    href = json.loads(body).get("href")
    return req(token, "GET", href)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--rate", type=int, default=0,
                    help="режим A4: N подряд запросов списка, счёт 429")
    args = ap.parse_args()

    token = os.environ.get("TOKEN", "")
    if not token:
        print("TOKEN=... не задан", file=sys.stderr)
        return 2

    if args.rate:
        n429 = 0
        t0 = time.time()
        for i in range(args.rate):
            st, _ = req(token, "GET", f"{API}/resources?path=%2Fecho&limit=1")
            if st == 429:
                n429 += 1
                print(f"{i + 1}: 429!")
            time.sleep(0.1)
        print(f"rate: {args.rate} запросов, 429: {n429}, "
              f"{time.time() - t0:.1f} c")
        return 0

    # --- основной smoke-цикл ---
    # 1. создать папку (409, если уже есть — не ошибка)
    st, _ = req(token, "PUT", f"{API}/resources?path={urllib.parse.quote(SMOKE_DIR)}")
    if st not in (201, 409):
        print(f"mkdir: неожиданный код {st}", file=sys.stderr)
        return 1

    # 2. загрузка overwrite=false
    st, _ = upload(token, f"{SMOKE_DIR}/hello.txt", HELLO.encode())
    up_code = st

    # 3. повторная загрузка по занятому имени: ожидаем 409 (или 423)
    st2, _ = upload(token, f"{SMOKE_DIR}/hello.txt", b"dup")
    # ФАКТ зафиксировать в docs/token-notes.md: 409 или 423?

    # 4. список папки
    st, body = req(token, "GET",
                   f"{API}/resources?path={urllib.parse.quote(SMOKE_DIR)}")
    names = [i["name"] for i in json.loads(body).get("_embedded", {}).get("items", [])] \
        if st == 200 else []
    lst = len(names)

    # 5. скачать и сравнить
    st, data = download(token, f"{SMOKE_DIR}/hello.txt")
    match = (data == HELLO.encode())

    # 6. удалить папку целиком
    st, _ = req(token, "DELETE", f"{API}/resources?path={urllib.parse.quote(SMOKE_DIR)}&permanently=true")
    dele = st

    ok = up_code in (201, 200) and lst == 1 and match and dele == 204
    print(f"{'ok' if ok else 'FAIL'}: list={lst} upload={up_code} "
          f"duplicate={st2} download={'match' if match else 'MISMATCH'} delete={dele}")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
