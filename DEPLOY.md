# Deploying Go Recording Service to Linux Server

## Step 1: Install Go on Ubuntu Server

```bash
# SSH into your server
ssh root@your-server-ip

# Install Go
apt update
apt install -y golang-go

# Verify installation
go version
# Should show: go version go1.21+ linux/amd64

# Set GOPATH (add to ~/.bashrc for persistence)
echo 'export GOPATH=$HOME/go' >> ~/.bashrc
echo 'export PATH=$PATH:$GOPATH/bin' >> ~/.bashrc
source ~/.bashrc
```

## Step 2: Install FFmpeg (for audio conversion)

```bash
apt install -y ffmpeg
ffmpeg -version
```

## Step 3: Upload Go Service to Server

From your local Windows machine:
```powershell
# Option 1: Using SCP
scp -r e:\flutter_app\go-recording-service root@your-server:/opt/

# Option 2: Using Git (if you push to a repo)
# On server:
cd /opt
git clone https://github.com/your-username/go-recording-service.git
```

## Step 4: Build the Go Binary

```bash
cd /opt/go-recording-service

# Download dependencies
go mod tidy

# Build the binary
go build -o recording-service .

# Test it manually first
./recording-service
# Should show: [GO-SFU] Starting on port 8081
# Press Ctrl+C to stop
```

## Step 5: Run with PM2

```bash
# Install PM2 (if not installed)
npm install -g pm2

# Start the Go binary with PM2
pm2 start ./recording-service --name "go-recording"

# Check status
pm2 status

# View logs
pm2 logs go-recording

# Auto-restart on server reboot
pm2 save
pm2 startup
```

## Step 6: Configure Nginx (Optional - for HTTPS)

Add to your nginx config:
```nginx
location /recording/ {
    proxy_pass http://127.0.0.1:8081/;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
}
```

Then reload nginx:
```bash
nginx -t
systemctl reload nginx
```

## Useful PM2 Commands

```bash
pm2 restart go-recording   # Restart
pm2 stop go-recording      # Stop
pm2 delete go-recording    # Remove from PM2
pm2 logs go-recording      # View logs
pm2 monit                  # Monitor all processes
```
