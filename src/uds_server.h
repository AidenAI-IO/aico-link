#pragma once

#include "uds_message.h"
#include <atomic>
#include <functional>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

namespace aico {

class UdsServer {
public:
    typedef std::function<void(const UdsMessage& request, int fd)> Handler;

    UdsServer(const char* socket_path, const Handler& handler);
    ~UdsServer();

    FrameServiceStatus start();
    void stop();

private:
    struct ClientWorker {
        std::thread thread;
        std::shared_ptr<std::atomic<bool> > done;
    };

    void accept_loop();
    void handle_client(int fd);
    void prune_client_threads_locked();

    std::string socket_path_;
    Handler handler_;
    int server_fd_;
    bool running_;
    std::thread accept_thread_;
    std::vector<ClientWorker> client_threads_;
    std::mutex mutex_;
};

}  // namespace aico
