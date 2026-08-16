FROM python:3.12-alpine
RUN pip install --no-cache-dir fastapi uvicorn
COPY main.py /app/main.py
WORKDIR /app
EXPOSE 9000
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "9000", "--log-level", "warning"]
