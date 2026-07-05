FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1
ENV PYTHONUNBUFFERED=1

WORKDIR /app

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY ledger_bot ./ledger_bot

RUN useradd --create-home --shell /usr/sbin/nologin botuser \
    && mkdir -p /app/data \
    && chown -R botuser:botuser /app

USER botuser

EXPOSE 8080

CMD ["python", "-m", "ledger_bot"]
