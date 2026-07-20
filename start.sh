docker stop 9gouter
docker rm 9gouter
docker build -t 9gouter .
docker run -d --name 9gouter -p 20128:20128 --env-file .env -v 9gouter-data:/app/data 9gouter