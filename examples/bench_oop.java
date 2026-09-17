interface Shape {
  long area();
}

class Rect implements Shape {
  private long w;
  private long h;
  public Rect(long w, long h) { this.w = w; this.h = h; }
  public long area() { return w * h; }
}

class Square extends Rect {
  public Square(long side) { super(side, side); }
}

public class bench_oop {
  public static void main(String[] args) {
    int rounds = 2000;
    if (args.length > 0) { rounds = Integer.parseInt(args[0]); }
    Shape[] shapes = new Shape[1000];
    for (int i = 0; i < 1000; i++) {
      shapes[i] = i % 2 == 0 ? new Rect(i, i + 1) : new Square(i);
    }
    long total = 0;
    for (int round = 0; round < rounds; round++) {
      for (int i = 0; i < 1000; i++) { total += shapes[i].area(); }
    }
    System.out.println(total);
  }
}
