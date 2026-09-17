public class bench_string_cjk {
  public static void main(String[] args) {
    int n = 200000;
    if (args.length > 0) { n = Integer.parseInt(args[0]); }
    String base = "中文";
    String emoji = "😀";

    long built = 0;
    for (int i = 0; i < n; i++) {
      String s = base + "-" + i + emoji;
      built += s.length();
    }

    StringBuilder sb = new StringBuilder();
    for (int i = 0; i < 1000; i++) { sb.append("中文"); }
    String text = sb.toString();
    long walked = 0;
    for (int i = 0; i < text.length(); i++) {
      if (text.charAt(i) == '中') { walked += 1; }
    }
    long sliced = text.substring(1, 5).length();
    System.out.println("concat=" + built + " walked=" + walked + " sliced=" + sliced);
  }
}
