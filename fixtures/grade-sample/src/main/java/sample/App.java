package sample;

public class App {
    private final Daypart daypart = new Daypart();
    private final Grader grader = new Grader();

    public String summary(int hour, int score) {
        return daypart.of(hour) + "/" + grader.of(score);
    }
}
