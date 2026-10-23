#include "uds_server.h"
#include <errno.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/un.h>
#include <utility>
#include <unistd.h>

namespace aico {

UdsServer::UdsServer(const char* socket_path, const Handler& handler)
    : socket_path_(socket_path ? socket_path : ""),
      handler_(handler),
      server_fd_(-1),
      running_(false) {}

UdsServer::~UdsServer() {
    stop();
}

FrameServiceStatus UdsServer::start() {
    if (socket_path_.empty()) {
        return FrameServiceStatus::INTERNAL_ERROR;
    }

    std::lock_guard<std::mutex> lock(mutex_);
    if (running_) {
        return FrameServiceStatus::OK;
    }

    server_fd_ = ::socket(AF_UNIX, SOCK_STREAM, 0);
    if (server_fd_ < 0) {
        return FrameServiceStatus::TRANSPORT_ERROR;
    }

    ::unlink(socket_path_.c_str());
    struct sockaddr_un addr;
    memset(&addr, 0, sizeof(addr));
    addr.sun_family = AF_UNIX;
    if (socket_path_.size() >= sizeof(addr.sun_path)) {
        ::close(server_fd_);
        server_fd_ = -1;
        return FrameServiceStatus::TRANSPORT_ERROR;
    }
    strncpy(addr.sun_path, socket_path_.c_str(), sizeof(addr.sun_path) - 1);
    if (::bind(server_fd_, reinterpret_cast<struct sockaddr*>(&addr), sizeof(addr)) < 0 ||
        ::listen(server_fd_, 16) < 0) {
        ::close(server_fd_);
        server_fd_ = -1;
        ::unlink(socket_path_.c_str());
        return FrameServiceStatus::TRANSPORT_ERROR;
    }

    // UNIX sockets inherit the process umask (0755 with Debian's default),
    // which prevents the non-root device user from reaching the service.  Set
    // the intended API mode explicitly; the systemd units provide the
    // matching `aico` group ownership.
    if (::chmod(socket_path_.c_str(), S_IRUSR | S_IWUSR | S_IRGRP | S_IWGRP) != 0) {
        ::close(server_fd_);
        server_fd_ = -1;
        ::unlink(socket_path_.c_str());
        return FrameServiceStatus::TRANSPORT_ERROR;
    }

    running_ = true;
    accept_thread_ = std::thread(&UdsServer::accept_loop, this);
    return FrameServiceStatus::OK;
}

void UdsServer::stop() {
    {
        std::lock_guard<std::mutex> lock(mutex_);
        if (!running_ && server_fd_ < 0) {
            return;
        }
        running_ = false;
        if (server_fd_ >= 0) {
            ::shutdown(server_fd_, SHUT_RDWR);
            ::close(server_fd_);
            server_fd_ = -1;
        }
    }
    if (accept_thread_.joinable()) {
        accept_thread_.join();
    }
    std::vector<ClientWorker> client_threads;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        client_threads.swap(client_threads_);
    }
    for (size_t i = 0; i < client_threads.size(); ++i) {
        if (client_threads[i].thread.joinable()) {
            client_threads[i].thread.join();
        }
    }
    ::unlink(socket_path_.c_str());
}

void UdsServer::prune_client_threads_locked() {
    for (size_t i = 0; i < client_threads_.size();) {
        if (client_threads_[i].done && client_threads_[i].done->load()) {
            if (client_threads_[i].thread.joinable()) {
                client_threads_[i].thread.join();
            }
            client_threads_.erase(client_threads_.begin() + i);
        } else {
            ++i;
        }
    }
}

void UdsServer::accept_loop() {
    while (true) {
        int fd = ::accept(server_fd_, nullptr, nullptr);
        if (fd < 0) {
            std::lock_guard<std::mutex> lock(mutex_);
            if (!running_) return;
            if (errno == EINTR) continue;
            continue;
        }
        std::lock_guard<std::mutex> lock(mutex_);
        if (!running_) {
            ::close(fd);
            return;
        }
        prune_client_threads_locked();
        ClientWorker worker;
        worker.done.reset(new std::atomic<bool>(false));
        std::shared_ptr<std::atomic<bool> > done = worker.done;
        worker.thread = std::thread([this, fd, done]() {
            handle_client(fd);
            done->store(true);
        });
        client_threads_.push_back(std::move(worker));
    }
}

void UdsServer::handle_client(int fd) {
    struct timeval timeout;
    timeout.tv_sec = 5;
    timeout.tv_usec = 0;
    ::setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    ::setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));

    while (true) {
        UdsMessage request;
        FrameServiceStatus read_status = read_uds_message(fd, &request);
        if (read_status != FrameServiceStatus::OK) {
            break;
        }
        if (handler_) {
            handler_(request, fd);
        }
    }
    ::close(fd);
}

}  // namespace aico
