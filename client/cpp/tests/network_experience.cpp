#include "roost/client_trace.hpp"
#include "roost/network_time.hpp"
#include "roost/prediction.hpp"
#include <iostream>
void check(bool ok) {
  if (!ok)
    throw std::runtime_error("network experience assertion");
}
int main() {
  roost::network_clock clock;
  check(clock.observe({0, .02, 100.01, 100.01}));
  check(std::abs(clock.now(1) - 101) < 1e-9);
  check(!clock.observe({1, 1.5, 101.1, 101.1}));
  check(clock.observe({2, 2.02, 101.91, 101.91}));
  double before = clock.now(2.02);
  check(clock.now(2.03) >= before);
  bool refused = false;
  try {
    clock.now(2);
  } catch (const std::invalid_argument &) {
    refused = true;
  }
  check(refused);
  clock.reset();
  check(!clock.ready());
  roost::snapshot_buffer<double> snapshots(2, 0, .2);
  snapshots.add(1, 1, 0);
  snapshots.add(1.1, 1.1, 10);
  check(!snapshots.add(1, 1.2, 100));
  auto lerp = [](double a, double b, double t) { return a + (b - a) * t; };
  double value = 0;
  check(snapshots.sample_at(1.15, lerp, value) && std::abs(value - 5) < 1e-8);
  snapshots.add(1.2, 2, 20);
  check(snapshots.size() == 2 && snapshots.delay() <= .2);
  snapshots.sample_at(100, lerp, value);
  check(value == 20);
  snapshots.add(1.3, 100, 30);
  snapshots.sample_at(1.45, lerp, value);
  check(std::abs(value - 25) < 1e-8);
  snapshots.clear();
  check(!snapshots.sample_at(100, lerp, value));
  roost::prediction_history<int> history(2);
  history.record(1, 1);
  history.record(2, 2);
  history.record(3, 3);
  auto clone = [](int x) { return x; };
  auto step = [](int s, int i) { return s + i; };
  check(history.held() && history.replay(10, clone, step) == 10);
  int last = 0;
  history.acknowledge(1, last);
  check(!history.held() && history.replay(10, clone, step) == 15);
  history.acknowledge(3, last);
  check(last == 3 && history.size() == 0);
  roost::client_trace trace(2);
  for (uint32_t i = 1; i <= 3; ++i)
    trace.record({roost::client_stage::apply, 1, i, double(i)});
  auto events = trace.drain();
  check(trace.overwritten() == 1 && events.size() == 2 && events[0].tick == 2 &&
        trace.drain().empty());
  std::cout << "PASS native clock/interpolation/prediction/trace\n";
}
